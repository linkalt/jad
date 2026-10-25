package native

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSweepStaleScratchRemovesOnlyOldPrivateScratchDirectories(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()
	old := now.Add(-8 * time.Hour)
	make := func(name string, mode os.FileMode, modTime time.Time) string {
		path := filepath.Join(tmp, name)
		if err := os.Mkdir(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "result.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		return path
	}
	staleExec := make("exec-abc", 0o700, old)
	staleSmith := make("tool-smith-abc", 0o700, old)
	fresh := make("exec-fresh", 0o700, now.Add(-time.Hour))
	notPrivate := make("exec-shared", 0o755, old)
	foreign := make("other-abc", 0o700, old)
	file := filepath.Join(tmp, "exec-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "exec-link")
	if err := os.Symlink(fresh, link); err != nil {
		t.Skip("symlinks are not available: ", err)
	}

	if removed := sweepStaleScratch(tmp, now, staleScratchAge); removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	for _, gone := range []string{staleExec, staleSmith} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s was not removed", gone)
		}
	}
	for _, kept := range []string{fresh, notPrivate, foreign, file, link} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

func TestBuildingExecSweepsStaleScratchOnceAtStartup(t *testing.T) {
	tmp := useOwnTempDir(t)
	stale := filepath.Join(tmp, "exec-crashed")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	sweepScratchOnce = sync.Once{}
	if _, err := NewExecTool(execTestSettings(), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the stale directory was kept")
	}
}
