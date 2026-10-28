package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreDoesNotDeleteThroughRecreatedSymlink(t *testing.T) {
	for _, mode := range []string{ModeSafe, ModeFull} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			outside := filepath.Join(filepath.Dir(f.workspace), "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			precious := filepath.Join(outside, "x")
			if err := os.WriteFile(precious, []byte("PRECIOUS\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../outside", f.path("data")); err != nil {
				t.Fatal(err)
			}
			f.write(t, "keep.txt", "k\n")
			id := f.begin(t, "turn-1", 1)
			if err := os.Remove(f.path("data")); err != nil {
				t.Fatal(err)
			}
			f.write(t, "data/x", "agent\n")
			f.service.Finish(id)
			if _, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: mode}); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if got, err := os.ReadFile(precious); err != nil || string(got) != "PRECIOUS\n" {
				t.Fatalf("file outside the workspace changed: %q, %v", got, err)
			}
		})
	}
}
