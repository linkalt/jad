package openaiauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/openaioauth"
)

var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func oldTokens(t *testing.T) map[string]any {
	return map[string]any{
		"id_token": "old-id", "access_token": accessJWT(t, fixedNow.Add(-48*time.Hour), time.Hour, "acct-old"),
		"refresh_token": "old-refresh", "account_id": "acct-old",
	}
}

func TestRefreshRewritesTheFileAndKeepsOtherKeys(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), map[string]any{"OPENAI_API_KEY": "sk-keep-me", "custom": map[string]any{"a": 1}})
	access := accessJWT(t, fixedNow, 10*24*time.Hour, "acct-new")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "new-id", "new-refresh")})

	result, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: func() time.Time { return fixedNow }})
	if err != nil || !result.Refreshed {
		t.Fatalf("refresh failed: %+v %v", result, err)
	}
	file := readJSON(t, store.Path)
	tokens := tokensOf(file)
	if tokens["access_token"] != access || tokens["refresh_token"] != "new-refresh" || tokens["id_token"] != "new-id" || tokens["account_id"] != "acct-new" {
		t.Fatalf("tokens not replaced: keys %v", keysOf(tokens))
	}
	if file["auth_mode"] != "chatgpt" || file["last_refresh"] != "2026-10-08T12:00:00.000Z" {
		t.Fatalf("auth_mode / last_refresh wrong: %v %v", file["auth_mode"], file["last_refresh"])
	}
	if file["OPENAI_API_KEY"] != "sk-keep-me" || file["custom"] == nil {
		t.Fatal("unknown top-level keys must be kept")
	}
	if got := issuer.Calls[0].JSON["refresh_token"]; got != "old-refresh" {
		t.Fatalf("the stored refresh token was not the one sent: %v", got)
	}
}

func keysOf(object map[string]any) []string {
	var keys []string
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

func TestRefreshKeepsUnknownTokenKeysAndOldValuesTheResponseOmits(t *testing.T) {
	store := storeIn(t)
	tokens := oldTokens(t)
	tokens["future_key"] = "x"
	writeFile(t, store, tokens, nil)
	access := accessJWT(t, fixedNow, time.Hour, "acct-old")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "")}) // no id_token, no refresh_token
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: func() time.Time { return fixedNow }}); err != nil {
		t.Fatal(err)
	}
	got := tokensOf(readJSON(t, store.Path))
	if got["future_key"] != "x" || got["id_token"] != "old-id" || got["refresh_token"] != "old-refresh" || got["access_token"] != access {
		t.Fatalf("omitted values must keep their old content: keys %v", keysOf(got))
	}
}

// What this tool writes must be loadable by the reader the backend uses.
func TestWrittenFileIsLoadableByTheBackendReader(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	access := accessJWT(t, time.Now(), 24*time.Hour, "acct-9")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "r2")})
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	source := &openaioauth.Source{Path: store.Path, ExpiryMargin: 5 * time.Minute}
	credential, err := source.Load()
	if err != nil {
		t.Fatalf("the backend reader refuses the file: %v", err)
	}
	if credential.AccessToken != access || credential.AccountID != "acct-9" {
		t.Fatal("the backend reader got other values than were stored")
	}
}

func TestFileModeAndBackup(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	before, _ := os.ReadFile(store.Path)
	if err := os.Chmod(store.Path, 0o644); err != nil { // an open file is made private again
		t.Fatal(err)
	}
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: func() time.Time { return fixedNow }}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.Path, store.BackupPath()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v err %v, want 0600", path, info.Mode().Perm(), err)
		}
	}
	backup, _ := os.ReadFile(store.BackupPath())
	if string(backup) != string(before) {
		t.Fatal("the backup must hold the file as it was before the rewrite")
	}
	entries, _ := os.ReadDir(filepath.Dir(store.Path))
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestNoBackupWhenDisabledOrWhenThereWasNoFile(t *testing.T) {
	store := storeIn(t)
	store.KeepBackup = false
	writeFile(t, store, oldTokens(t), nil)
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.BackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("keep_backup false must not write a backup")
	}
}

func TestFailedRefreshLeavesTheFileAndNoBackupBehind(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	before, _ := os.ReadFile(store.Path)
	for name, response := range map[string]reply{
		"rejected":     {400, `{"error":"invalid_grant"}`},
		"server error": {503, `{}`},
		"bad body":     {200, `nope`},
		"no account":   {200, tokenBody(t, jwt(t, map[string]any{"exp": fixedNow.Add(time.Hour).Unix()}), "", "r")},
	} {
		if name == "no account" {
			// The old file's account id is kept, so a response without one is still storable. Remove it
			// to exercise the check.
			writeFile(t, store, map[string]any{"access_token": "x", "refresh_token": "old-refresh"}, nil)
			before, _ = os.ReadFile(store.Path)
		}
		issuer := newFakeIssuer(t, response)
		_, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{})
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
		after, _ := os.ReadFile(store.Path)
		if string(after) != string(before) {
			t.Errorf("%s: the credential file changed after a failed refresh", name)
		}
	}
	if _, err := os.Stat(store.BackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed refresh must not write a backup")
	}
}

func TestRefreshNeedsLoginWhenThereIsNothingToRefresh(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t)
	client := clientFor(t, issuer)
	if _, err := Refresh(context.Background(), store, client, RefreshOptions{}); !errors.Is(err, ErrNeedLogin) {
		t.Fatalf("missing file: want ErrNeedLogin, got %v", err)
	}
	if _, err := os.Stat(filepath.Dir(store.Path)); err == nil {
		if _, err := os.Stat(store.Path); err == nil {
			t.Fatal("a refresh without a file must not create one")
		}
	}
	writeFile(t, store, map[string]any{"access_token": "a", "account_id": "x"}, nil)
	if _, err := Refresh(context.Background(), store, client, RefreshOptions{}); !errors.Is(err, ErrNeedLogin) {
		t.Fatalf("no refresh token: want ErrNeedLogin, got %v", err)
	}
	if issuer.callCount() != 0 {
		t.Fatal("no request may be sent without a refresh token")
	}
}

func TestRejectedRefreshTokenMeansLoginAgain(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	issuer := newFakeIssuer(t, reply{400, `{"error":"invalid_grant","error_description":"expired"}`}, reply{503, `{}`})
	_, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{})
	if !errors.Is(err, ErrNeedLogin) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("rejected token: want ErrNeedLogin with the code, got %v", err)
	}
	_, err = Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{})
	if err == nil || errors.Is(err, ErrNeedLogin) {
		t.Fatalf("a 503 must not tell the user to log in again, got %v", err)
	}
}

func TestIfOlderThan(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), map[string]any{"last_refresh": fixedNow.Add(-10 * time.Minute).Format(time.RFC3339)})
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	client := clientFor(t, issuer)
	now := func() time.Time { return fixedNow }

	result, err := Refresh(context.Background(), store, client, RefreshOptions{IfOlderThan: time.Hour, Now: now})
	if err != nil || result.Refreshed || !result.AgeKnown || result.Age != 10*time.Minute {
		t.Fatalf("recent tokens must be skipped: %+v %v", result, err)
	}
	if issuer.callCount() != 0 {
		t.Fatal("a skipped refresh must not send a request")
	}
	if _, err := os.Stat(store.BackupPath()); err == nil {
		t.Fatal("a skipped refresh must not touch the backup")
	}
	result, err = Refresh(context.Background(), store, client, RefreshOptions{IfOlderThan: 5 * time.Minute, Now: now})
	if err != nil || !result.Refreshed || issuer.callCount() != 1 {
		t.Fatalf("old enough: must refresh: %+v %v", result, err)
	}
	// An unreadable last_refresh counts as due: refreshing too often is safe, too rarely is not.
	writeFile(t, store, oldTokens(t), map[string]any{"last_refresh": "garbage"})
	issuer2 := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	result, err = Refresh(context.Background(), store, clientFor(t, issuer2), RefreshOptions{IfOlderThan: time.Hour, Now: now})
	if err != nil || !result.Refreshed || result.AgeKnown {
		t.Fatalf("unreadable last_refresh must refresh: %+v %v", result, err)
	}
}

// Two refreshes at once must not both spend the stored refresh token.
func TestConcurrentRefreshesAreSerialised(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	var inFlight, peak atomic.Int32
	var mutex sync.Mutex
	used := map[string]int{}
	issuer := newCountingIssuer(t, func(refresh string) (int, string) {
		now := inFlight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		mutex.Lock()
		defer mutex.Unlock()
		used[refresh]++
		if used[refresh] > 1 {
			return 400, `{"error":"invalid_grant","error_description":"refresh token already used"}`
		}
		return 200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "acct-old"), "", fmt.Sprintf("rotated-%d", len(used)))
	})
	client := clientFor(t, issuer)
	var wait sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, errs[i] = Refresh(context.Background(), store, client, RefreshOptions{})
		}()
	}
	wait.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d refresh requests were in flight at once; the lock must allow one", peak.Load())
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("refresh %d failed although each one saw the previous one's rotated token: %v", i, err)
		}
	}
}

func TestUpdateWaitsForTheLockAndHonoursCancel(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = store.Update(context.Background(), func(current *File) (*File, error) {
			close(held)
			<-release
			return nil, errSkip
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := store.Update(ctx, func(*File) (*File, error) { t.Error("change must not run without the lock"); return nil, errSkip })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error while the lock is held, got %v", err)
	}
	close(release)
}

func TestReadErrorsDoNotQuoteTheFile(t *testing.T) {
	store := storeIn(t)
	if _, err := store.Read(); !errors.Is(err, ErrNoFile) {
		t.Fatalf("want ErrNoFile, got %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"tokens": SECRET-GARBAGE`, `[1,2]`, `{"tokens":"SECRET-STRING"}`} {
		if err := os.WriteFile(store.Path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.Read()
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%q: want an error that does not quote the file, got %v", body, err)
		}
	}
}

func TestFileNeverPrints(t *testing.T) {
	file := File{AccessToken: "SECRET-A", IDToken: "SECRET-I", RefreshToken: "SECRET-R"}
	if text := fmt.Sprintf("%v %+v %#v %s", file, file, file, &file); strings.Contains(text, "SECRET") {
		t.Fatalf("a token printed: %s", text)
	}
}
