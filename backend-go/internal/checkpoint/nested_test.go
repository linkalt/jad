package checkpoint

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestNestedGitRepositoryIsLeftOutAndWarned(t *testing.T) {
	for _, committed := range []bool{false, true} {
		f := newFixture(t, nil)
		f.write(t, "keep.txt", "k0\n")
		f.write(t, "vend/f.txt", "v0\n")
		script := "cd " + f.path("vend") + " && git init -q"
		if committed {
			script += " && git add . && git -c user.name=a -c user.email=a@b commit -qm c"
		}
		if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
		id := f.begin(t, "turn-1", 1)
		if id == "" {
			t.Fatalf("committed=%v: no checkpoint was taken beside a nested repository: %+v", committed, f.service.List())
		}
		f.write(t, "keep.txt", "agent\n")
		f.service.Finish(id)
		res, err := f.service.Restore(context.Background(), id, RestoreOptions{Mode: ModeFull})
		if err != nil {
			t.Fatal(err)
		}
		if f.read(t, "keep.txt") != "k0\n" {
			t.Errorf("committed=%v: keep.txt was not restored", committed)
		}
		if !strings.Contains(res.Warning, "vend") {
			t.Errorf("committed=%v: restore did not warn about the nested repository: %q", committed, res.Warning)
		}
		var undo Record
		for _, r := range f.service.List() {
			if r.ID == res.UndoCheckpointID {
				undo = r
			}
		}
		if !strings.Contains(undo.Warning, "vend") {
			t.Errorf("committed=%v: the restore record carries no warning: %q", committed, undo.Warning)
		}
	}
}
