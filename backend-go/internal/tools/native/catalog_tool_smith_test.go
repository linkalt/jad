package native

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func poolNames(pool []tools.Tool) []string {
	var names []string
	for _, tool := range pool {
		names = append(names, tool.Definition.Name)
	}
	return names
}

var sampleToolSmithManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json")

func toolSmithConfig(t *testing.T) sessionfs.SessionConfig {
	t.Helper()
	return sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{sampleToolSmithManifest},
		NativeToolSmith:     &sessionfs.NativeToolSmithSettings{},
	}
}

func TestBuildCatalogLoadsConfiguredToolSmith(t *testing.T) {
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != toolSmithToolID || definitions[0].Name != "tool_smith" {
		t.Fatalf("tool_smith catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogOmitsToolSmithWhenItIsNotConfigured(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: "setting.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Definitions()) != 0 {
		t.Fatalf("an unconfigured catalog exposes %v", catalog.Definitions())
	}
	if output := executeCatalogCall(t, catalog, "tool_smith", `{}`); !strings.Contains(output, "error_code: unknown_tool") {
		t.Fatalf("an unconfigured tool_smith call = %s", output)
	}
}

func TestBuildCatalogRequiresTheToolSmithManifest(t *testing.T) {
	config := toolSmithConfig(t)
	config.ToolDefinitionPaths = nil
	_, err := BuildCatalog(config)
	if err == nil || !strings.Contains(err.Error(), "tools.native.tool_smith is configured but native.tool_smith is missing from tool_definition_paths") {
		t.Fatalf("missing tool_smith manifest error = %v", err)
	}
}

func TestToolSmithIsNeverInASubagentPool(t *testing.T) {
	smith, err := NewToolSmithTool(sessionfs.NativeToolSmithSettings{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !smith.ReadOnly {
		t.Fatal("tool_smith must be ReadOnly for the exclusion to matter")
	}
	readOnly, changers := childToolPools(sessionfs.NativeSubagentSettings{AllowWrite: true}, []tools.Tool{readOnlyTool("read"), smith})
	for _, tool := range append(readOnly, changers...) {
		if tool.Definition.ID == toolSmithToolID {
			t.Fatalf("a sub-agent pool contains tool_smith: %v", poolNames(readOnly))
		}
	}
	if len(readOnly) != 1 || readOnly[0].Definition.Name != "read" {
		t.Fatalf("read-only pool = %v, want read", poolNames(readOnly))
	}
}

func TestToolSmithChangesStateOnlyForARun(t *testing.T) {
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		arguments string
		want      bool
	}{
		"run object":          {`{"run":{"function":"return 1;"}}`, true},
		"run string":          {`{"run":"return 1;"}`, true},
		"run with alias code": {`{"run":{"code":"return 1;"}}`, true},
		"run with alias body": {`{"run":{"body":"return 1;"}}`, true},
		"run wins over list":  {`{"list":{},"run":"return 1;"}`, true},
		"list":                {`{"list":{"filters":["a"]}}`, false},
		"empty list":          {`{"list":{}}`, false},
		"nothing":             {`{}`, false},
	} {
		inner, _ := json.Marshal(tc.arguments)
		call := json.RawMessage(`{"type":"function_call","call_id":"c","name":"tool_smith","arguments":` + string(inner) + `}`)
		if got := catalog.ChangesState(call); got != tc.want {
			t.Errorf("%s: ChangesState = %v, want %v", name, got, tc.want)
		}
	}
}

func TestToolSmithCatalogListFailsWithoutFunctionsDir(t *testing.T) {
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	output := executeCatalogCall(t, catalog, "tool_smith", `{"list":{}}`)
	if !strings.Contains(output, "error_code: list_failed") {
		t.Errorf("tool_smith list = %s", output)
	}
}
