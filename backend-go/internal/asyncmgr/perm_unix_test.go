//go:build unix

package asyncmgr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAsyncFolderIsOwnerOnlyAndAnExistingOneIsTightenedAtOpen(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "o", "a")
	f.finish(t, "a", Result{})
	f.m.Close()
	if err := os.Chmod(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	f.m.Close()
	info, err := os.Stat(f.dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("folder = %v, %v, want 0700", info, err)
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) < 2 {
		t.Fatalf("expected record and index files, got %d", len(entries))
	}
	for _, entry := range entries {
		if fi, err := entry.Info(); err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s = %v, %v, want no group/other access", entry.Name(), fi, err)
		}
	}
}

func TestNewAsyncFolderIsCreatedOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", ".async")
	if err := mkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("folder = %v, %v, want 0700", info, err)
	}
}
