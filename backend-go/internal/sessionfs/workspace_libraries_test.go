package sessionfs

import (
	"path/filepath"
	"slices"
	"testing"
)

// global.json's libraries resolve from G and workspace.json's from W/.jad; a kind not set is
// nil (the built-in default), one set to [] is empty; a session carries its workspace's
// libraries. workspace.json takes only the folder keys.
func TestWorkspaceLibrariesAreReadFromBothFiles(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"libraries":{"prompt_dirs":["prompts","/abs/p"],"skill_dirs":[]}}`)
	f.globalJSON(t, `{"libraries":{"skill_dirs":["skills"],"max_asset_bytes":4096}}`)
	config := wsMustLoad(t, f, a)
	libraries := config.Roots[0].Layout.Libraries
	if libraries == nil || libraries.GlobalDir != f.global || libraries.MaxAssetBytes != 4096 {
		t.Fatalf("libraries = %+v", libraries)
	}
	if !slices.Equal(libraries.Global.Skills, []string{filepath.Join(f.global, "skills")}) || libraries.Global.Prompts != nil {
		t.Fatalf("global = %+v", libraries.Global)
	}
	own := libraries.Workspace
	if !slices.Equal(own.Prompts, []string{filepath.Join(a, ".jad", "prompts"), "/abs/p"}) || own.Skills == nil || len(own.Skills) != 0 || own.Commands != nil {
		t.Fatalf("workspace = %+v", own)
	}
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Libraries != libraries {
		t.Fatal("the session does not carry its workspace's libraries")
	}
	for _, refused := range []string{`{"libraries":{"max_asset_bytes":2048}}`, `{"libraries":{"tool_definition_paths":[]}}`, `{"libraries":{"prompt_dirs":"p"}}`, `{"libraries":{"prompt_dirs":[""]}}`} {
		wsPut(t, filepath.Join(a, ".jad", "workspace.json"), refused)
		if _, err := f.load(a); err == nil {
			t.Errorf("workspace.json %s is accepted", refused)
		}
	}
	wsPut(t, filepath.Join(a, ".jad", "workspace.json"), `{}`)
	for _, refused := range []string{`{"libraries":{"agents":[]}}`, `{"libraries":{"max_asset_bytes":0}}`, `{"libraries":[]}`} {
		f.globalJSON(t, refused)
		if _, err := f.load(a); err == nil {
			t.Errorf("global.json %s is accepted", refused)
		}
	}
}
