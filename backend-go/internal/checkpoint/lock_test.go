package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStaleIndexLockDoesNotDisableCheckpoints(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "a.txt", "a0\n")
	id := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "agent\n")
	f.service.Finish(id)
	for _, name := range []string{"snapshot.index.lock", "restore.index.lock"} {
		if err := os.WriteFile(filepath.Join(f.staging, GitDirectoryName, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if next := f.service.Begin(context.Background(), "turn-2", 2); next == "" {
		t.Fatalf("Begin took no checkpoint with a stale lock: %+v", f.service.List())
	}
	for _, name := range []string{"snapshot.index.lock", "restore.index.lock"} {
		if err := os.WriteFile(filepath.Join(f.staging, GitDirectoryName, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull}); err != nil {
		t.Fatalf("Restore with stale locks: %v", err)
	}
	if got := f.read(t, "a.txt"); got != "a0\n" {
		t.Fatalf("a.txt = %q, want a0", got)
	}
}
