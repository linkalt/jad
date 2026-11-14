package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLibraryDefinitionGlobs(t *testing.T) {
	for _, source := range []string{"global", "workspace"} {
		t.Run(source, func(t *testing.T) {
			f := newWSFixture(t)
			// Both source directories contain literal metacharacters.
			f.global = filepath.Join(f.home, "[g].jad")
			// Load locates G through JAD_GLOBAL_DIR below.
			wsPut(t, filepath.Join(f.global, "models/openai/default.json"), `{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_KEY"}`)
			a := f.workspace(t, "w[1]", `{}`)
			base := f.global
			if source == "workspace" {
				base = filepath.Join(a, ".jad")
			}
			for _, name := range []string{"b.json", "a.json", "long.json", "nested/c.json"} {
				wsPut(t, filepath.Join(base, "defs", name), `{}`)
			}
			outside := filepath.Join(f.base, "external", "d.json")
			wsPut(t, outside, `{}`)
			entries := []string{"defs/b.json", "defs/*.json", "defs/?.json", "defs/[ab].json", filepath.Join(f.base, "external", "*.json"), "defs/missing.json", "defs/../defs/a.json", "defs/**.json"}
			raw, _ := json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": entries}})
			f.globalJSON(t, `{}`)
			if source == "global" {
				f.globalJSON(t, string(raw))
			} else {
				wsPut(t, filepath.Join(a, ".jad/workspace.json"), string(raw))
			}
			config, err := Load(LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": f.home, "JAD_GLOBAL_DIR": f.global}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(base, "defs/b.json"), filepath.Join(base, "defs/a.json"), filepath.Join(base, "defs/long.json"), outside, filepath.Join(base, "defs/missing.json")}
			if got := config.Roots[0].Layout.ToolDefinitionPaths; !slices.Equal(got, want) {
				t.Fatalf("got %q want %q", got, want)
			}
			session, err := wsSession(t, config, "s1", "")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(session.ToolDefinitionPaths, want) {
				t.Fatalf("session: %q", session.ToolDefinitionPaths)
			}
			// Pin each syntax independently: overlapping entries must not hide a
			// question mark treated like a star, or a class treated as a literal.
			for _, tc := range []struct {
				pattern string
				names   []string
			}{
				{"defs/?.json", []string{"defs/a.json", "defs/b.json"}},
				{"defs/[b].json", []string{"defs/b.json"}},
				{"defs/**.json", []string{"defs/a.json", "defs/b.json", "defs/long.json"}},
				{"defs/*/*.json", []string{"defs/nested/c.json"}},
				{"defs/../defs/*.json", []string{"defs/a.json", "defs/b.json", "defs/long.json"}},
			} {
				raw, err := json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{tc.pattern}}})
				if err != nil {
					t.Fatal(err)
				}
				if source == "global" {
					f.globalJSON(t, string(raw))
				} else {
					wsPut(t, filepath.Join(a, ".jad/workspace.json"), string(raw))
				}
				config, err := Load(LoadOptions{WorkingDir: a, Environment: map[string]string{"HOME": f.home, "JAD_GLOBAL_DIR": f.global}})
				if err != nil {
					t.Fatal(err)
				}
				want := make([]string, len(tc.names))
				for i, name := range tc.names {
					want[i] = filepath.Join(base, name)
				}
				if got := config.Roots[0].Layout.ToolDefinitionPaths; !slices.Equal(got, want) {
					t.Fatalf("%s: got %q want %q", tc.pattern, got, want)
				}
			}
		})
	}
}

func TestLibraryDefinitionGlobMerge(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{}`)
	shared := filepath.Join(f.base, "shared")
	for _, name := range []string{"b.json", "a.json"} {
		wsPut(t, filepath.Join(shared, name), `{}`)
	}
	global, _ := json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{filepath.Join(shared, "*.json")}}})
	own, _ := json.Marshal(map[string]any{"libraries": map[string]any{"tool_definition_paths": []string{filepath.Join(shared, "?.json"), "missing.json"}}})
	f.globalJSON(t, string(global))
	wsPut(t, filepath.Join(a, ".jad/workspace.json"), string(own))
	config := wsMustLoad(t, f, a)
	want := []string{filepath.Join(shared, "a.json"), filepath.Join(shared, "b.json"), filepath.Join(a, ".jad/missing.json")}
	l := config.Roots[0].Layout
	if !slices.Equal(l.ToolDefinitionPaths, want) || !slices.Equal(l.WorkspaceToolDefinitions, want) {
		t.Fatalf("merged %q own %q want %q", l.ToolDefinitionPaths, l.WorkspaceToolDefinitions, want)
	}
}

func TestLibraryDefinitionPathErrors(t *testing.T) {
	f := newWSFixture(t)
	a := f.workspace(t, "empty", `{"libraries":{"tool_definition_paths":[]}}`)
	f.globalJSON(t, `{"libraries":{"tool_definition_paths":[]}}`)
	if got := wsMustLoad(t, f, a).Roots[0].Layout.ToolDefinitionPaths; len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}

	for _, source := range []string{"global", "workspace"} {
		for _, tc := range []struct{ raw, want string }{
			{`null`, "must be an array"}, {`"x"`, "must be an array"}, {`{}`, "must be an array"},
			{`["ok",null]`, "[1] must be a non-empty string"}, {`["ok",3]`, "[1] must be a non-empty string"},
			{`["ok",""]`, "[1] must be a non-empty string"}, {`["ok","  "]`, "[1] must be a non-empty string"},
			{`["ok","absent/["]`, "[1] invalid pattern \"absent/[\""},
			{`["ok","absent/*.json"]`, "[1] pattern \"absent/*.json\" matched no paths"},
		} {
			t.Run(source+tc.raw, func(t *testing.T) {
				f := newWSFixture(t)
				a := f.workspace(t, "a", `{}`)
				f.globalJSON(t, `{}`)
				text := `{"libraries":{"tool_definition_paths":` + tc.raw + `}}`
				where := filepath.Join(f.global, "global.json")
				if source == "global" {
					f.globalJSON(t, text)
				} else {
					where = filepath.Join(a, ".jad/workspace.json")
					wsPut(t, where, text)
				}
				_, err := f.load(a)
				if err == nil || !strings.Contains(err.Error(), where) || !strings.Contains(err.Error(), "libraries.tool_definition_paths"+tc.want) && !strings.Contains(err.Error(), "libraries.tool_definition_paths "+tc.want) {
					t.Fatalf("want source %s and %s: %v", where, tc.want, err)
				}
			})
		}
	}
}

func TestLibraryDefinitionConcreteWildcard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain * or ?; backslash is a separator")
	}
	f := newWSFixture(t)
	a := f.workspace(t, "a", `{"libraries":{"tool_definition_paths":["defs/*.json","defs/literal\\*.json"]}}`)
	for _, name := range []string{"literal*.json", "literalOther.json"} {
		wsPut(t, filepath.Join(a, ".jad/defs", name), `{}`)
	}
	f.globalJSON(t, `{}`)
	session, err := wsSession(t, wsMustLoad(t, f, a), "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(a, ".jad/defs/literal*.json"), filepath.Join(a, ".jad/defs/literalOther.json")}
	if !slices.Equal(session.ToolDefinitionPaths, want) {
		t.Fatalf("got %q want %q", session.ToolDefinitionPaths, want)
	}
	// Unix backslashes in a literal base must be escaped too.
	base := filepath.Join(t.TempDir(), `base\[?*`)
	wsPut(t, filepath.Join(base, "a.json"), `{}`)
	got, err := parseLibraryDefinitionPaths(json.RawMessage(`["*.json"]`), "test", base)
	if err != nil || !slices.Equal(got, []string{filepath.Join(base, "a.json")}) {
		t.Fatalf("base escaping: %q %v", got, err)
	}
}

func TestLibraryDefinitionPathSchemas(t *testing.T) {
	for _, name := range []string{"global.schema.json", "workspace.schema.json"} {
		schema := compileSchema(t, name)
		for _, raw := range []string{`[]`, `["defs/*.json","defs/?.json","defs/[ab].json"]`} {
			if !schemaAccepts(t, schema, `{"libraries":{"tool_definition_paths":`+raw+`}}`) {
				t.Errorf("%s refuses %s", name, raw)
			}
		}
		for _, raw := range []string{`null`, `"x"`, `{}`, `[null]`, `[1]`, `[""]`, `["  "]`} {
			if schemaAccepts(t, schema, `{"libraries":{"tool_definition_paths":`+raw+`}}`) {
				t.Errorf("%s accepts %s", name, raw)
			}
		}
	}
}
