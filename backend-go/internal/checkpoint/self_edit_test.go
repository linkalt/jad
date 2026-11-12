package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// A checkpoint taken before profile self-edit's folders were left out (an old checkpoint) still
// holds them; a restore with RestoreKeep leaves them, in either mode, restores the rest, says
// so, and the next snapshot no longer holds them (3b.2).
func TestAnOldCheckpointKeepsSelfEditFoldersOnRestore(t *testing.T) {
	f := newFixture(t, nil)
	f.seed(t)
	f.write(t, ".jad/profile-own/dev/coder/documents/notes.md", "n0\n")
	f.write(t, ".jad/profiles/dev/coder.json", "{\"v\":0}\n")
	f.write(t, ".jad/workspace.json", "{}\n")
	old := f.begin(t, "turn-1", 1)
	f.write(t, "a.txt", "a1\n")
	f.write(t, ".jad/profile-own/dev/coder/documents/notes.md", "n1\n")
	f.write(t, ".jad/profile-own/dev/coder/history/000001-abc.json", "{}\n")
	f.write(t, ".jad/profiles/dev/coder.json", "{\"v\":1}\n")
	f.write(t, ".jad/workspace.json", "{\"x\":1}\n")
	f.service.Finish(old)

	keep := []string{f.path(".jad/profile-own"), f.path(".jad/profiles")}
	upgraded := NewService(Options{Settings: defaultSettings(), Workspace: f.workspace, StagingDir: f.staging,
		HarnessPaths: append([]string{f.path(".tasks")}, keep...), RestoreKeep: keep, Durability: "process"})
	for _, mode := range []string{ModeSafe, ModeFull} {
		f.write(t, "a.txt", "a1\n")
		f.write(t, ".jad/workspace.json", "{\"x\":1}\n")
		result, err := upgraded.Restore(context.Background(), old, RestoreOptions{Mode: mode})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if f.read(t, "a.txt") != "a0\n" || f.read(t, ".jad/workspace.json") != "{}\n" {
			t.Fatalf("%s: the workspace was not restored: %+v", mode, result)
		}
		if f.read(t, ".jad/profile-own/dev/coder/documents/notes.md") != "n1\n" || f.read(t, ".jad/profiles/dev/coder.json") != "{\"v\":1}\n" ||
			!f.exists(".jad/profile-own/dev/coder/history/000001-abc.json") {
			t.Fatalf("%s: a self-edit folder was restored: %+v", mode, result)
		}
		for _, path := range append(append(result.Restored, result.Deleted...), result.Conflicts...) {
			if strings.HasPrefix(path, ".jad/profile") {
				t.Fatalf("%s: the result names %s: %+v", mode, path, result)
			}
		}
		if !strings.Contains(result.Warning, "profile self-edit owns") {
			t.Fatalf("%s: warning %q", mode, result.Warning)
		}
	}
	current, err := upgraded.capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listing, err := upgraded.git.run(context.Background(), []string{"ls-tree", "-r", "--name-only", current.tree}, runOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(listing), ".jad/profile") || !strings.Contains(string(listing), ".jad/workspace.json") {
		t.Fatalf("the new snapshot holds:\n%s", listing)
	}
}

// A path at, below or above a kept folder (a file or link that would replace the folder) is
// dropped from a plan; a sibling is not.
func TestPlanKeepDropsKeptPathsAndTheirParents(t *testing.T) {
	workspace := t.TempDir()
	changes := plan{
		restore:   []TreeChange{{Path: ".jad"}, {Path: ".jad/profile-own/x"}, {Path: ".jad/profile-owner"}, {Path: "a.txt"}},
		remove:    []TreeChange{{Path: ".jad/profiles/dev/c.json"}, {Path: "b.txt"}},
		conflicts: []string{".jad/profile-own", ".jad/workspace.json"},
	}
	dropped := changes.keep(workspace, []string{filepath.Join(workspace, ".jad", "profile-own"), filepath.Join(workspace, ".jad", "profiles"), "/elsewhere/.jad/profile-own"})
	if dropped != 4 || !reflect.DeepEqual(paths(changes.restore), []string{".jad/profile-owner", "a.txt"}) ||
		!reflect.DeepEqual(paths(changes.remove), []string{"b.txt"}) || !reflect.DeepEqual(changes.conflicts, []string{".jad/workspace.json"}) {
		t.Fatalf("dropped %d, plan %+v", dropped, changes)
	}
	if again := (&plan{restore: []TreeChange{{Path: "a"}}}).keep(workspace, nil); again != 0 {
		t.Fatalf("no kept paths dropped %d", again)
	}
}

// The self-edit folders are left out and kept by the names the workspace gives them, also when
// the workspace is reached through a link; v1 (no SelfEditPaths) adds nothing.
func TestSelfEditPathsAreHarnessPathsUnderTheWorkspacesName(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, ".jad", "profile-own"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	config := sessionfs.SessionConfig{WorkingDir: link, SelfEditPaths: []string{filepath.Join(real, ".jad", "profile-own"), filepath.Join(real, ".jad", "profiles")}}
	got := selfEditPaths(config)
	for _, want := range []string{filepath.Join(link, ".jad", "profile-own"), filepath.Join(link, ".jad", "profiles")} {
		if !containsPath(got, want) || !containsPath(HarnessPaths(config), want) {
			t.Fatalf("selfEditPaths = %v, HarnessPaths = %v, want %s", got, HarnessPaths(config), want)
		}
	}
	if paths := ForSession(config, nil).options.RestoreKeep; !reflect.DeepEqual(paths, got) {
		t.Fatalf("RestoreKeep = %v, want %v", paths, got)
	}
	config.SelfEditPaths = nil
	if got := selfEditPaths(config); got != nil {
		t.Fatalf("v1 selfEditPaths = %v", got)
	}
}
