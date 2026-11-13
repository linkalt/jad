package sessionfs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTargetFixture is workspace proj with dev/coder, a toolset per write target in W/.jad/tools
// and a link W/notes to the profile's own folder.
func newWriteTargetFixture(t *testing.T) editFixture {
	t.Helper()
	e := newEditFixture(t, `{"profile_self_edit":"full"}`, `{"profile_self_edit":"full"}`, editBase+`}`)
	own := filepath.Join(e.w, ".jad", "profile-own", "dev", "coder")
	wsPut(t, filepath.Join(own, "documents", "doc.md"), "doc")
	wsPut(t, filepath.Join(e.w, ".jad", "profile-own", "dev", "other", "documents", "doc.md"), "other")
	if err := os.Symlink(own, filepath.Join(e.w, "notes")); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"records":   `{"native":{"records":{"storage_dir":".jad/profile-own/dev/coder/documents"}}}`,
		"task":      `{"native":{"task":{"storage_dir":"notes/tasks"}}}`,
		"functions": `{"native":{"exec":{"functions_dir":".jad/profile-own/dev/coder/documents"}}}`,
		"spill":     `{"native":{"exec":{"spill":{"stdout_full":"{cwd}/.jad/profile-own/dev/coder/out-{call_id}.txt","stderr_full":"{cwd}/.jad/state/err-{call_id}.txt"}}}}`,
		"lawful":    `{"native":{"records":{"storage_dir":".jad/data/profiles/dev/coder/records"}}}`,
	} {
		wsPut(t, filepath.Join(e.w, ".jad", "tools", "t", name+".json"), text)
	}
	return e
}

// Hand-written profile files: every in-process write target that lands in a profile-own folder
// fails the load where the loader resolved it, whether it is relative, goes through .., starts
// with a placeholder or runs through a link; lawful targets still load.
func TestWriteTargetsInAProfileOwnFolderFailTheLoad(t *testing.T) {
	e := newWriteTargetFixture(t)
	refused := map[string]string{
		"relative":       `"settings":{"tool_storage_dir":".jad/profile-own/dev/coder/documents"}`,
		"dot-dot":        `"settings":{"tool_storage_dir":"src/../.jad/profile-own/dev/other"}`,
		"placeholder":    `"settings":{"tool_storage_dir":"{profile_data}/../.jad/profile-own/dev/coder"}`,
		"link":           `"settings":{"tool_storage_dir":"notes/state"}`,
		"image store":    `"settings":{"image_store":{"dir":"` + filepath.Join(e.w, "notes", "images") + `"}}`,
		"records":        `"toolset":["t/records"]`,
		"task by a link": `"toolset":["t/task"]`,
		"functions":      `"toolset":["t/functions"]`,
		"spill file":     `"toolset":["t/spill"]`,
	}
	for name, value := range refused {
		wsPut(t, e.file, editBase+`,`+value+`}`)
		if _, err := LoadSessionSettings(e.location, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "profile-own folder") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, value := range map[string]string{
		"state":      `"settings":{"tool_storage_dir":".jad/state2"}`,
		"data":       `"settings":{"tool_storage_dir":".jad/data/profiles/dev/coder/tools"}`,
		"toolset":    `"toolset":["t/lawful"]`,
		"no setting": `"description":"plain"`,
	} {
		wsPut(t, e.file, editBase+`,`+value+`}`)
		if _, err := LoadSessionSettings(e.location, "dev/coder", ""); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A self-edit meets the same refusal: through the early text check for a setting, and through
// the trial load for a target the text does not show (a toolset's storage); nothing is written.
func TestProfileEditRefusesWriteTargetsInAProfileOwnFolder(t *testing.T) {
	e := newWriteTargetFixture(t)
	before, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"relative":    `"settings":{"tool_storage_dir":".jad/profile-own/dev/coder/documents"}`,
		"dot-dot":     `"settings":{"tool_storage_dir":"src/../.jad/profile-own/dev/other"}`,
		"placeholder": `"settings":{"tool_storage_dir":"{profile_data}/../.jad/profile-own/dev/coder"}`,
		"link":        `"settings":{"tool_storage_dir":"notes/state"}`,
		"records":     `"toolset":["t/records"]`,
		"task":        `"toolset":["t/task"]`,
		"functions":   `"toolset":["t/functions"]`,
	} {
		if _, err := e.edit(t, editBase+`,`+value+`}`); err == nil || !strings.Contains(err.Error(), "profile-own folder") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after, err := os.ReadFile(e.file); err != nil || string(after) != string(before) {
		t.Fatalf("the profile file changed: %s, %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(e.w, ".jad", "profile-own", "dev", "coder", "history")); err == nil {
		t.Fatal("a refused edit left a history entry")
	}
	if _, err := e.edit(t, editBase+`,"toolset":["t/lawful"]}`); err != nil {
		t.Fatal(err)
	}
}

// A v2 load names what profile self-edit owns for checkpoints (every workspace's profile-own and
// this workspace's profile catalog), and gives exec its profile-own roots for the per-call check
// of a worktree run (3b.2); agent toolsets get them too.
func TestAWorkspaceLoadNamesSelfEditPathsAndGuardsChildWrites(t *testing.T) {
	e := newWriteTargetFixture(t)
	wsPut(t, filepath.Join(e.w, ".jad", "tools", "t", "exec.json"), `{"native":{"exec":{"spill":{"storage_dir":".jad/state/spill"}}}}`)
	wsPut(t, e.file, editBase+`,"toolset":["t/exec"]}`)
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(e.w, ".jad", "profile-own")
	if !slices.Contains(config.SelfEditPaths, own) || !slices.Contains(config.SelfEditPaths, filepath.Join(e.w, ".jad", "profiles")) {
		t.Fatalf("SelfEditPaths = %v", config.SelfEditPaths)
	}
	if config.NativeExec == nil || !slices.Contains(config.NativeExec.Child.ProfileOwnRoots, own) {
		t.Fatalf("exec roots = %+v", config.NativeExec)
	}
	derived := SessionConfig{NativeToolSmith: &NativeToolSmithSettings{}, AgentToolsets: map[string]SessionConfig{"a": {NativeExec: &NativeExecSettings{}}}}
	derived.guardChildWrites([]string{own})
	if !slices.Equal(derived.NativeToolSmith.Child.ProfileOwnRoots, []string{own}) || !slices.Equal(derived.AgentToolsets["a"].NativeExec.Child.ProfileOwnRoots, []string{own}) {
		t.Fatalf("guarded = %+v", derived)
	}
}
