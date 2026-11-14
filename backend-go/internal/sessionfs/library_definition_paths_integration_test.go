package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLibraryDefinitionGlobGlobalOnly(t *testing.T) {
	f := newWSFixture(t)
	// A workspace is required, but no listed workspaces or workspace library is.
	cwd := f.workspace(t, "project", `{}`)
	first := filepath.Join(f.global, "defs/a.json")
	wsPut(t, first, `{}`)
	f.globalJSON(t, `{"libraries":{"tool_definition_paths":["defs/*.json"]}}`)
	config := wsMustLoad(t, f, cwd)
	if got := config.Roots[0].Layout.ToolDefinitionPaths; !slices.Equal(got, []string{first}) {
		t.Fatalf("global-only: %q", got)
	}
	// With only a global library, matches still pass the forbidden-place check.
	wsPut(t, filepath.Join(f.global, "auth/secret.json"), `{}`)
	f.globalJSON(t, `{"libraries":{"tool_definition_paths":["auth/*.json"]}}`)
	if _, err := f.load(cwd); err == nil || !strings.Contains(err.Error(), "which tool definitions may not be read from") {
		t.Fatalf("global-only guard: %v", err)
	}
}

func TestLibraryDefinitionGlobSandboxOwnership(t *testing.T) {
	f := newWSFixture(t)
	shared := filepath.Join(f.base, "defs")
	a := f.workspace(t, "a", `{}`)
	for _, name := range []string{"a.json", "b.json"} {
		wsPut(t, filepath.Join(shared, name), `{}`)
	}
	// a.json overlaps with the global list; b.json belongs only to the workspace.
	global := wsSandboxDefaults(f, "")
	var setting map[string]any
	if err := json.Unmarshal([]byte(global), &setting); err != nil {
		t.Fatal(err)
	}
	setting["libraries"] = map[string]any{"tool_definition_paths": []string{filepath.Join(shared, "a*.json")}}
	data, err := json.Marshal(setting)
	if err != nil {
		t.Fatal(err)
	}
	f.globalJSON(t, string(data))
	data, err = json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{filepath.Join(shared, "*.json")}}})
	if err != nil {
		t.Fatal(err)
	}
	wsPut(t, filepath.Join(a, ".jad/workspace.json"), string(data))
	config := wsMustLoad(t, f, a)
	want := []string{filepath.Join(shared, "a.json"), filepath.Join(shared, "b.json")}
	if got := config.Roots[0].Layout.ToolDefinitionPaths; !slices.Equal(got, want) {
		t.Fatalf("merged %q", got)
	}
	session, err := wsSession(t, config, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Sandbox == nil {
		t.Fatal("no sandbox")
	}
	for _, path := range want {
		if !slices.Contains(session.Sandbox.ReadOnlyPaths, path) {
			t.Errorf("workspace match %s is not read-only: %q", path, session.Sandbox.ReadOnlyPaths)
		}
	}
}

func TestLibraryDefinitionGlobForbiddenPlaces(t *testing.T) {
	for _, source := range []string{"global", "workspace"} {
		for _, place := range []string{"auth", "other-workspace", "profile-own", "data", "profiles", "symlink-file", "symlink-directory"} {
			t.Run(source+"/"+place, func(t *testing.T) {
				f := newWSFixture(t)
				a := f.workspace(t, "a", `{}`)
				b := f.workspace(t, "b", `{}`)
				root := filepath.Join(a, ".jad")
				switch place {
				case "auth":
					root = filepath.Join(f.global, "auth")
				case "other-workspace", "symlink-file", "symlink-directory":
					root = filepath.Join(b, ".jad")
				case "profile-own":
					root = filepath.Join(root, "profile-own/dev/coder/documents")
				case "data":
					root = filepath.Join(root, "data/profiles/dev/coder")
				case "profiles":
					root = filepath.Join(root, "profiles/dev")
				}
				target := filepath.Join(root, "definition.json")
				wsPut(t, target, `{}`)
				pattern := filepath.Join(root, "*.json")
				if place == "symlink-file" {
					link := filepath.Join(f.base, "public/linked.json")
					if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, link); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
					pattern = filepath.Join(filepath.Dir(link), "*.json")
				} else if place == "symlink-directory" {
					link := filepath.Join(f.base, "public")
					if err := os.Symlink(root, link); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
					pattern = filepath.Join(f.base, "pub*", "*.json")
				}
				global := map[string]any{"workspaces": []any{map[string]any{"id": "a", "path": a}, map[string]any{"id": "b", "path": b}}}
				library := map[string]any{"tool_definition_paths": []string{pattern}}
				if source == "global" {
					global["libraries"] = library
				} else {
					data, err := json.Marshal(map[string]any{"libraries": library})
					if err != nil {
						t.Fatal(err)
					}
					wsPut(t, filepath.Join(a, ".jad/workspace.json"), string(data))
				}
				data, err := json.Marshal(global)
				if err != nil {
					t.Fatal(err)
				}
				f.globalJSON(t, string(data))
				_, err = f.load(a)
				if err == nil || !strings.Contains(err.Error(), "which tool definitions may not be read from") || !strings.Contains(err.Error(), root) {
					t.Fatalf("guard for %s: %v", pattern, err)
				}
			})
		}
	}
}
