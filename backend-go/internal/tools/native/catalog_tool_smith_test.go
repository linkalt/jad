package native

import (
	"encoding/json"
	"os"
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

// A child is a profile: tool_smith is in its set by default, and only its own allow list or a
// deny list (its own, or the profile's tools.deny) takes it away.
func TestToolSmithIsInEveryChildByDefaultAndOnlyADenyOrAllowListTakesItAway(t *testing.T) {
	smith, err := NewToolSmithTool(sessionfs.NativeToolSmithSettings{}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := childNamesOf(t, agentNamed("plain"), nil, readOnlyTool("read"), smith)
	if !containsName(plain, "tool_smith") {
		t.Fatalf("a child without a tools list did not get tool_smith: %v", plain)
	}
	listed := agentNamed("scripter")
	listed.Tools, listed.HasTools = []string{"read"}, true
	if chosen := childNamesOf(t, listed, nil, readOnlyTool("read"), smith); containsName(chosen, "tool_smith") {
		t.Fatalf("a child whose own allow list leaves tool_smith out got %v", chosen)
	}
	if chosen := childNamesOf(t, agentNamed("plain"), []string{"tool_smith"}, readOnlyTool("read"), smith); containsName(chosen, "tool_smith") {
		t.Fatalf("the profile's tools.deny did not take tool_smith from the child: %v", chosen)
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
		"code":                  {`{"code":"return 1;"}`, true},
		"function with input":   {`{"code":"return 1;","input":{"a":1}}`, true},
		"function with filters": {`{"filters":["a"],"code":"return 1;"}`, true},
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

// The description the model sees is written twice: the manifest sample and toolSmithDescription. They must stay
// equal, so an edit to one without the other fails here.
func TestToolSmithDescriptionCopiesAreEqual(t *testing.T) {
	data, err := os.ReadFile(sampleToolSmithManifest)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []struct {
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("tool_smith.json: %v, %d tools", err, len(document.Tools))
	}
	if got := document.Tools[0].Description; got != toolSmithDescription {
		t.Errorf("tool_smith.json description differs from toolSmithDescription:\njson: %q\ngo:   %q", got, toolSmithDescription)
	}
}
