package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// The schemas of global.json and workspace.json list exactly the keys the loader reads, and
// take and refuse what it takes and refuses.
func TestWorkspaceSchemasAgreeWithTheLoader(t *testing.T) {
	for name, keys := range map[string][]string{"global.schema.json": globalSettingKeys, "workspace.schema.json": workspaceSettingKeys} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		var got []string
		for key := range schema.Properties {
			got = append(got, key)
		}
		sort.Strings(got)
		want := slices.Clone(keys)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s properties %q, loader keys %q", name, got, want)
		}
	}
	global := compileSchema(t, "global.schema.json")
	if !schemaAccepts(t, global, `{"schema_version":2,"workspaces":[{"id":"a","path":"~/a"}],"catalogs":{"models":"m"},"defaults":{"max_run_tokens":10},"libraries":{"prompt_dirs":["p"],"max_asset_bytes":2048}}`) {
		t.Error("a valid global.json is refused")
	}
	for _, refused := range []string{`{"session_roots":[]}`, `{"workspaces":[{"id":"a","path":"rel"}]}`, `{"catalogs":{"profiles":"p"}}`, `{"schema_version":1}`} {
		if schemaAccepts(t, global, refused) {
			t.Errorf("global.json %s is accepted", refused)
		}
	}
	workspace := compileSchema(t, "workspace.schema.json")
	if !schemaAccepts(t, workspace, `{"schema_version":2,"id":"w","label":"W","defaults":{"cwd":"src"},"libraries":{"prompt_dirs":["prompts"],"skill_dirs":["skills"],"tool_definition_paths":["tools_defintion/native/exec.json"]}}`) {
		t.Error("a valid workspace.json is refused")
	}
	for _, refused := range []string{`{"catalogs":{}}`, `{"libraries":{"max_asset_bytes":2048}}`, `{"libraries":{"tool_definition_paths":"x.json"}}`, `{"libraries":{"tool_definition_paths":[""]}}`, `{"id":"bad id"}`} {
		if schemaAccepts(t, workspace, refused) {
			t.Errorf("workspace.json %s is accepted", refused)
		}
	}
}
