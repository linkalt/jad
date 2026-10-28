package checkpoint

import (
	"context"
	"os"
	"testing"
)

func TestRestoreWhenFileBecameDirectoryAndBack(t *testing.T) {
	for _, mode := range []string{ModeFull, ModeSafe} {
		f := newFixture(t, nil)
		f.write(t, "a/x", "x0\n")
		f.write(t, "b", "b0\n")
		f.write(t, "z.txt", "z0\n")
		id := f.begin(t, "turn-1", 1)
		os.RemoveAll(f.path("a"))
		f.write(t, "a", "now a file\n")
		os.Remove(f.path("b"))
		f.write(t, "b/y", "y\n")
		f.write(t, "z.txt", "agent\n")
		f.service.Finish(id)
		res, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: mode})
		if err != nil {
			t.Fatalf("%s: restore failed after doing its work: %v", mode, err)
		}
		if f.read(t, "a/x") != "x0\n" || f.read(t, "b") != "b0\n" || f.read(t, "z.txt") != "z0\n" {
			t.Errorf("%s: workspace not restored: %+v", mode, res)
		}
	}
}
