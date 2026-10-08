package claudeauth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func now() time.Time { return fixedNow }

func TestRefreshStoresNewTokens(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow.Add(-time.Minute)))
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc-new", "ref-new", 28800)})
	result, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: now})
	if err != nil || !result.Refreshed {
		t.Fatalf("result %+v err %v", result, err)
	}
	if issuer.Calls[0].JSON["refresh_token"] != "old-refresh" {
		t.Fatal("the stored refresh token was not sent")
	}
	got := readJSON(t, store.Path)
	if got["access_token"] != "acc-new" || got["refresh_token"] != "ref-new" || got["expires_at"] != "2026-10-08T20:00:00Z" {
		t.Fatalf("stored wrong: %v", keys(got))
	}
}

func TestRefreshSkipRules(t *testing.T) {
	cases := []struct {
		name    string
		options RefreshOptions
		expires time.Duration // from fixedNow
		skipped bool
	}{
		{"recent refresh is skipped", RefreshOptions{IfOlderThan: 2 * time.Hour}, time.Hour, true},
		{"old refresh runs", RefreshOptions{IfOlderThan: 30 * time.Minute}, time.Hour, false},
		{"token with time left is skipped", RefreshOptions{IfExpiresWithin: 10 * time.Minute}, time.Hour, true},
		{"token ending soon runs", RefreshOptions{IfExpiresWithin: 2 * time.Hour}, time.Hour, false},
		{"expired token runs", RefreshOptions{IfExpiresWithin: time.Minute}, -time.Hour, false},
		{"no rule always runs", RefreshOptions{}, 24 * time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := storeIn(t)
			writeFile(t, store, oldFile(fixedNow.Add(c.expires)))
			issuer := newFakeIssuer(t, reply{200, tokenBody("n", "r", 3600)})
			c.options.Now = now
			result, err := Refresh(context.Background(), store, clientFor(t, issuer), c.options)
			if err != nil {
				t.Fatal(err)
			}
			if result.Refreshed == c.skipped || (issuer.callCount() == 0) != c.skipped {
				t.Fatalf("refreshed=%v calls=%d, skipped want %v", result.Refreshed, issuer.callCount(), c.skipped)
			}
			if c.skipped && readJSON(t, store.Path)["access_token"] != "old-access" {
				t.Fatal("a skipped refresh changed the file")
			}
		})
	}
}

func TestRefreshWithoutFileOrRefreshTokenNeedsLogin(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t)
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: now}); !errors.Is(err, ErrNeedLogin) {
		t.Fatalf("missing file: %v", err)
	}
	file := oldFile(fixedNow)
	delete(file, "refresh_token")
	writeFile(t, store, file)
	if _, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: now}); !errors.Is(err, ErrNeedLogin) {
		t.Fatalf("no refresh token: %v", err)
	}
	if issuer.callCount() != 0 {
		t.Fatal("the issuer was called")
	}
}

func TestRefreshRejectionNeedsLoginAndKeepsTheFile(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow))
	before, _ := os.ReadFile(store.Path)
	issuer := newFakeIssuer(t, reply{400, `{"error":"invalid_grant"}`})
	_, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: now})
	if !errors.Is(err, ErrNeedLogin) {
		t.Fatalf("err %v", err)
	}
	if after, _ := os.ReadFile(store.Path); string(after) != string(before) {
		t.Fatal("a rejected refresh changed the file")
	}
}

func TestRefreshServerErrorIsNotALoginProblem(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow))
	issuer := newFakeIssuer(t, reply{503, `{}`})
	_, err := Refresh(context.Background(), store, clientFor(t, issuer), RefreshOptions{Now: now})
	if err == nil || errors.Is(err, ErrNeedLogin) {
		t.Fatalf("err %v", err)
	}
}

// Two concurrent refreshes must not spend the rotating refresh token twice: the second one runs
// after the first stored the new token and (with a skip rule) does nothing.
func TestConcurrentRefreshesSpendTheTokenOnce(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow.Add(-time.Minute)))
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc-new", "ref-new", 3600)}, reply{400, `{"error":"invalid_grant"}`})
	client := clientFor(t, issuer)
	var wait sync.WaitGroup
	errs := make([]error, 2)
	for index := range errs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, errs[index] = Refresh(context.Background(), store, client, RefreshOptions{IfExpiresWithin: 10 * time.Minute, Now: now})
		}()
	}
	wait.Wait()
	if errs[0] != nil || errs[1] != nil || issuer.callCount() != 1 {
		t.Fatalf("errors %v, issuer calls %d, want 1 call and no error", errs, issuer.callCount())
	}
}
