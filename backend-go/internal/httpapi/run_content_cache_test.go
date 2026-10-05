package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunContentCacheSpillsRawTextAndReplaysSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "responses.partial")
	cache := newRunContentCache(4, 4, time.Hour)
	cache.SetPartialPath(path)
	cache.Append("ab")
	cache.Append("cde")
	cache.Append("fgh")

	if got := cache.Snapshot(); got != "abcdefgh" {
		t.Fatalf("partial snapshot = %q, want %q", got, "abcdefgh")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "abcdefgh" {
		t.Fatalf("partial file = %q, error=%v", got, err)
	}
	ownerBytes, err := os.ReadFile(path + runPartialOwnerSuffix)
	if err != nil {
		t.Fatal(err)
	}
	var owner partialOwner
	if err := json.Unmarshal(ownerBytes, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.PID != os.Getpid() || owner.Hostname == "" {
		t.Fatalf("partial owner = %+v", owner)
	}

	cache.Close(true)
	for _, file := range []string{path, path + runPartialOwnerSuffix} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("partial cache file was not removed: %s, error=%v", file, err)
		}
	}
}

func TestRunContentCachePreservesInterleavedTextPartOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.partial")
	cache := newRunContentCache(1, 2, 0)
	cache.SetPartialPath(path)
	defer cache.Close(true)
	for _, delta := range []string{"A1", "B1", "A2"} {
		cache.Append(delta)
	}
	if got := cache.Snapshot(); got != "A1B1A2" {
		t.Fatalf("interleaved text snapshot = %q, want A1B1A2", got)
	}
}

func TestRunContentCacheFallsBackToMemoryOnUnwritablePath(t *testing.T) {
	root := t.TempDir()
	parentFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parentFile, "responses.partial")
	cache := newRunContentCache(1, 2, 0)
	cache.SetPartialPath(path)
	cache.Append("a")
	cache.Append("b")
	cache.Append("c")
	if got := cache.Snapshot(); got != "abc" {
		t.Fatalf("fallback snapshot = %q, want abc", got)
	}
	if got, err := os.ReadFile(parentFile); err != nil || string(got) != "sentinel" {
		t.Fatalf("fallback modified unrelated file: %q, error=%v", got, err)
	}
	cache.Close(true)
}

func TestRunContentCacheDoesNotRemoveForeignPartialOnOwnerCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "responses.partial")
	ownerPath := path + runPartialOwnerSuffix
	if err := os.WriteFile(path, []byte("foreign-cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, []byte(`{"hostname":"other","pid":123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := newRunContentCache(0, 2, 0)
	cache.SetPartialPath(path)
	cache.Append("new")
	if got := cache.Snapshot(); got != "new" {
		t.Fatalf("fallback snapshot = %q, want new", got)
	}
	for file, want := range map[string]string{path: "foreign-cache", ownerPath: `{"hostname":"other","pid":123}`} {
		if got, err := os.ReadFile(file); err != nil || string(got) != want {
			t.Errorf("foreign file %s changed: content=%q error=%v", file, got, err)
		}
	}
	cache.Close(true)
}
