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

// Flipped on purpose (nesting, design_nesting_and_approval.md): tool_smith is in the pool a
// child draws from, but like async it reaches a child only when its tools list names it.
func TestToolSmithIsInTheSubagentPoolButOnlyAnAgentThatListsItGetsIt(t *testing.T) {
	smith, err := NewToolSmithTool(sessionfs.NativeToolSmithSettings{}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !smith.ReadOnly {
		t.Fatal("tool_smith must be ReadOnly to be in the read-only pool")
	}
	readOnly, _ := childToolPools(sessionfs.NativeSubagentSettings{}, []tools.Tool{readOnlyTool("read"), smith})
	if !containsName(poolNames(readOnly), "tool_smith") {
		t.Fatalf("read-only pool = %v, want tool_smith in it", poolNames(readOnly))
	}
	plain, _, err := subagentChildTools(agentNamed("plain"), readOnly, nil)
	if err != nil || containsName(poolNames(plain), "tool_smith") {
		t.Fatalf("a child without a tools list got tool_smith: %v %v", poolNames(plain), err)
	}
	listed := agentNamed("scripter")
	listed.Tools, listed.HasTools = []string{"read", "tool_smith"}, true
	chosen, _, err := subagentChildTools(listed, readOnly, nil)
	if err != nil || !containsName(poolNames(chosen), "tool_smith") {
		t.Fatalf("a child that lists tool_smith got %v, %v", poolNames(chosen), err)
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
		"function":              {`{"function":"return 1;"}`, true},
		"function with input":   {`{"function":"return 1;","input":{"a":1}}`, true},
		"function with filters": {`{"filters":["a"],"function":"return 1;"}`, true},
		"list with filters":     {`{"filters":["a"]}`, false},
		"list wildcard":         {`{"filters":["read*"]}`, false},
		"nothing":               {`{}`, false},
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
	output := executeCatalogCall(t, catalog, "tool_smith", `{}`)
	if !strings.Contains(output, "error_code: list_failed") {
		t.Errorf("tool_smith list = %s", output)
	}
}

func TestToolSmithDescriptionStatesTheScriptIsNotSandboxed(t *testing.T) {
	const want = "full interpreter privileges"
	catalog, err := BuildCatalog(toolSmithConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Definitions()[0].Description; !strings.Contains(got, want) {
		t.Errorf("manifest description = %q, want it to contain %q", got, want)
	}
	if !strings.Contains(toolSmithDescription, want) {
		t.Errorf("toolSmithDescription = %q, want it to contain %q", toolSmithDescription, want)
	}
}
