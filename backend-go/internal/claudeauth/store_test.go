package claudeauth

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestApplyStoresTheExpiryAndKeepsUnknownKeys(t *testing.T) {
	store := storeIn(t)
	file := oldFile(fixedNow.Add(time.Hour))
	file["future_key"] = map[string]any{"a": 1}
	writeFile(t, store, file)
	err := store.Update(context.Background(), func(current *File) (*File, error) {
		return current.Apply(&Tokens{AccessToken: "new-access", ExpiresIn: 7200}, fixedNow)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, store.Path)
	if got["access_token"] != "new-access" || got["refresh_token"] != "old-refresh" || got["scope"] != "old-scope" ||
		got["expires_at"] != "2026-10-08T14:00:00Z" || got["last_refresh"] != "2026-10-08T12:00:00Z" || got["version"] != float64(1) {
		t.Fatalf("stored file wrong, keys %v", keys(got))
	}
	if _, ok := got["future_key"]; !ok {
		t.Fatal("unknown keys must be kept")
	}
	backup := readJSON(t, store.BackupPath())
	if backup["access_token"] != "old-access" {
		t.Fatal("the backup must hold the previous file")
	}
}

func TestFileIsPrivate(t *testing.T) {
	store := storeIn(t)
	err := store.Update(context.Background(), func(current *File) (*File, error) {
		return current.Apply(&Tokens{AccessToken: "a", RefreshToken: "r", ExpiresIn: 60}, fixedNow)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.Path} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v err %v", path, info.Mode(), err)
		}
	}
	dir, _ := os.Stat(store.Path[:strings.LastIndex(store.Path, "/")])
	if dir.Mode().Perm()&0o077 != 0 {
		t.Fatalf("directory is open to others: %v", dir.Mode())
	}
}

func TestNoBackupWhenDisabledAndNoTempFilesLeft(t *testing.T) {
	store := storeIn(t)
	store.KeepBackup = false
	writeFile(t, store, oldFile(fixedNow.Add(time.Hour)))
	_ = store.Update(context.Background(), func(current *File) (*File, error) {
		return current.Apply(&Tokens{AccessToken: "n", ExpiresIn: 60}, fixedNow)
	})
	entries, _ := os.ReadDir(store.Path[:strings.LastIndex(store.Path, "/")])
	for _, entry := range entries {
		if entry.Name() != "claude-oauth.json" && !strings.HasSuffix(entry.Name(), ".jad-lock") {
			t.Errorf("unexpected file %s left behind", entry.Name())
		}
	}
	if _, err := os.Stat(store.BackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a backup was written although keep_backup is off")
	}
}

func TestUpdateWritesNothingWhenChangeFails(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow.Add(time.Hour)))
	before, _ := os.ReadFile(store.Path)
	boom := errors.New("boom")
	if err := store.Update(context.Background(), func(*File) (*File, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(store.Path)
	if string(before) != string(after) {
		t.Fatal("the file changed")
	}
	if _, err := os.Stat(store.BackupPath()); err == nil {
		t.Fatal("a backup was written for a failed update")
	}
}

func TestReadErrors(t *testing.T) {
	store := storeIn(t)
	if _, err := store.Read(); !errors.Is(err, ErrNoFile) {
		t.Fatalf("missing file: %v", err)
	}
	_ = os.MkdirAll(store.Path[:strings.LastIndex(store.Path, "/")], 0o700)
	_ = os.WriteFile(store.Path, []byte(`["SECRET-IN-FILE"`), 0o600)
	_, err := store.Read()
	if err == nil || strings.Contains(err.Error(), "SECRET-IN-FILE") {
		t.Fatalf("a parse error must not quote the file: %v", err)
	}
}

func TestApplyRejectsAFileTheAdapterCannotLoad(t *testing.T) {
	if _, err := (*File)(nil).Apply(&Tokens{AccessToken: "", ExpiresIn: 60}, fixedNow); err == nil {
		t.Error("an empty access token was accepted")
	}
}
