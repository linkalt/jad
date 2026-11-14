package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// Load roots and session settings before building the real native catalog, as the
// app's acquireCatalogParts does. The definition reader itself stays unchanged.
func TestLibraryDefinitionGlobsReachNativeCatalog(t *testing.T) {
	for _, scenario := range []string{"distinct-and-overlap", "duplicate-id", "invalid-document", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			f := layouttest.Write(t, layouttest.Spec{Native: map[string]any{"skill": map[string]any{}, "async": map[string]any{}}})
			defs := filepath.Join(f.Workspace, "definitions")
			if err := os.MkdirAll(defs, 0o755); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(defs, "a.json"), filepath.Join(defs, "b.json")}
			for i, name := range []string{"async", "skill"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample/tools/native", name+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths[i], data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON(f.GlobalSetting, map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{filepath.Join(defs, "a*.json")}}})
			writeJSON(f.WorkspaceSetting, map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{"../definitions/*.json"}}})
			bad := filepath.Join(defs, "c.json")
			switch scenario {
			case "duplicate-id":
				data, err := os.ReadFile(paths[1])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bad, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid-document":
				writeJSON(bad, map[string]any{"not_tools": true})
			case "directory":
				if err := os.Mkdir(bad, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			roots, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.Workspace, GlobalDir: f.GlobalDir})
			if err != nil {
				t.Fatal(err)
			}
			location, err := sessionfs.Resolve(roots.Roots, f.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			config, err := sessionfs.LoadSessionSettings(location, "", "")
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(paths)
			if scenario != "distinct-and-overlap" {
				want = append(want, bad)
			}
			if !slices.Equal(config.ToolDefinitionPaths, want) {
				t.Fatalf("expanded concrete paths %q, want %q", config.ToolDefinitionPaths, want)
			}
			catalog, err := BuildCatalog(config)
			if scenario == "distinct-and-overlap" {
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, definition := range catalog.Definitions() {
					names = append(names, definition.Name)
				}
				if !slices.Contains(names, "skill") || !slices.Contains(names, "async") {
					t.Fatalf("native tools not exposed: %q", names)
				}
				// Contents of an already discovered file are re-read on the next build.
				writeJSON(paths[1], map[string]any{"invalid": true})
				if _, err := BuildCatalog(config); err == nil || !strings.Contains(err.Error(), paths[1]+": invalid tool definition document") {
					t.Fatalf("content reread: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("bad matched entry was silently ignored")
			}
			for _, part := range map[string][]string{
				"duplicate-id":     {bad + ": duplicate tool definition native.skill", "also defined in " + paths[1]},
				"invalid-document": {bad + ": invalid tool definition document"},
				"directory":        {bad + ": could not read or parse tool definitions"},
			}[scenario] {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%v lacks %q", err, part)
				}
			}
		})
	}
}
