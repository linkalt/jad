package sessionfs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// global.json's libraries resolve from G and workspace.json's from W/.jad; a kind not set is
// nil (the built-in default), one set to [] is empty; a session carries its workspace's
// libraries. workspace.json takes the folder keys and tool_definition_paths (4c).
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
	for _, refused := range []string{`{"libraries":{"max_asset_bytes":2048}}`, `{"libraries":{"tool_definition_paths":"x.json"}}`, `{"libraries":{"tool_definition_paths":[""]}}`, `{"libraries":{"tool_definition_paths":null}}`, `{"libraries":{"prompt_dirs":"p"}}`, `{"libraries":{"prompt_dirs":[""]}}`} {
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

// 4c: workspace.json lists tool definition files of its own, resolved from W/.jad and read
// after the global list; the same file in both is read once; another workspace does not see
// them; every sandbox of the workspace keeps them read-only; v1 is unchanged.
func TestWorkspaceToolDefinitionPathsFollowTheGlobalOnes(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"libraries":{"tool_definition_paths":["tools_defintion/b.json","/abs/c.json","../shared/d.json"]}}`)
	b := f.workspace(t, "b", `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"libraries":{"tool_definition_paths":["defs/a.json","/abs/c.json"]}}`)
	wantA := []string{filepath.Join(f.global, "defs/a.json"), "/abs/c.json", filepath.Join(a, ".jad", "tools_defintion/b.json"), filepath.Join(a, "shared/d.json")}
	config := wsMustLoad(t, f, a)
	layout := config.Roots[0].Layout
	if !slices.Equal(layout.ToolDefinitionPaths, wantA) {
		t.Fatalf("a: %q, want the global ones then the workspace's: %q", layout.ToolDefinitionPaths, wantA)
	}
	if own := []string{filepath.Join(a, ".jad", "tools_defintion/b.json"), "/abs/c.json", filepath.Join(a, "shared/d.json")}; !slices.Equal(layout.WorkspaceToolDefinitions, own) {
		t.Fatalf("workspace's own: %q, want %q", layout.WorkspaceToolDefinitions, own)
	}
	session, err := wsSession(t, config, "a:s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(session.ToolDefinitionPaths, wantA) {
		t.Fatalf("session: %q", session.ToolDefinitionPaths)
	}
	// The other workspace reads only the global list.
	inB := wsMustLoad(t, f, b)
	if got := inB.Roots[0].Layout.ToolDefinitionPaths; !slices.Equal(got, []string{filepath.Join(f.global, "defs/a.json"), "/abs/c.json"}) {
		t.Fatalf("b: %q", got)
	}
}

func TestWorkspaceToolDefinitionPathsAreReadOnlyInTheSandbox(t *testing.T) {
	f := newWSFixture(t)
	outside := filepath.Join(f.base, "defs")
	a := f.workspace(t, "a", `{"libraries":{"tool_definition_paths":["`+filepath.Join(f.base, "defs/native.json")+`","tools_defintion/n.json"]}}`)
	f.globalJSON(t, wsSandboxDefaults(f, ""))
	config := wsMustLoad(t, f, a)
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Sandbox == nil {
		t.Fatal("no sandbox")
	}
	for _, want := range []string{filepath.Join(outside, "native.json"), filepath.Join(a, ".jad", "tools_defintion/n.json")} {
		if !slices.Contains(session.Sandbox.ReadOnlyPaths, want) {
			t.Errorf("read-only %q lacks %s", session.Sandbox.ReadOnlyPaths, want)
		}
	}
}

// 4c: a tool definition file may not lie where the fixed hidden list or the folders a profile
// writes are, as written or through a link; the folders of the workspace and G are fine.
func TestToolDefinitionPathsRefusePlacesTheLoaderKeepsAway(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	b := f.workspace(t, "b", `{}`)
	link := filepath.Join(f.base, "link")
	if err := os.Symlink(filepath.Join(b, ".jad"), link); err != nil {
		t.Fatal(err)
	}
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}]}`)
	cases := []struct {
		name, setting, want string
	}{
		{"G/auth", `"` + filepath.Join(f.global, "auth", "x.json") + `"`, "auth"},
		{"another workspace's .jad", `"` + filepath.Join(b, ".jad", "t.json") + `"`, filepath.Join(b, ".jad")},
		{"through a link", `"` + filepath.Join(link, "t.json") + `"`, filepath.Join(b, ".jad")},
		{"own profile-own", `"profile-own/dev/coder/documents/t.json"`, "profile-own"},
		{"data of a profile", `"data/profiles/dev/coder/t.json"`, filepath.Join(a, ".jad", "data")},
		{"profiles folder", `"profiles/dev/t.json"`, filepath.Join(a, ".jad", "profiles")},
		{"a missing file below another's .jad", `"../../b/.jad/new/t.json"`, filepath.Join(b, ".jad")},
	}
	for _, test := range cases {
		wsPut(t, filepath.Join(a, ".jad", "workspace.json"), `{"libraries":{"tool_definition_paths":[`+test.setting+`]}}`)
		_, err := f.load(a)
		if err == nil || !strings.Contains(err.Error(), "tool_definition_paths") || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v", test.name, err)
		}
	}
	// The same refusal for a path in global.json.
	wsPut(t, filepath.Join(a, ".jad", "workspace.json"), `{}`)
	f.globalJSON(t, `{"workspaces":[{"id":"a","path":"`+a+`"},{"id":"b","path":"`+b+`"}],"libraries":{"tool_definition_paths":["`+filepath.Join(a, ".jad", "profile-own", "t.json")+`"]}}`)
	if _, err := f.load(a); err == nil {
		t.Error("a global path into profile-own is accepted")
	}
	f.globalJSON(t, `{"libraries":{"tool_definition_paths":["tools/n.json"]}}`)
	wsPut(t, filepath.Join(a, ".jad", "workspace.json"), `{"libraries":{"tool_definition_paths":["tools_defintion/n.json","tools/w.json","../tools/x.json"]}}`)
	if _, err := f.load(a); err != nil {
		t.Errorf("places in the workspace and in G are refused: %v", err)
	}
}
