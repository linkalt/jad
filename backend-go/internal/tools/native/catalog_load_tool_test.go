package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var sampleLoadToolManifest = filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "load_tool.json")

// load_tool is built only when tools.native.load_tool is enabled, and from its definition file.
func TestCatalogBuildsLoadToolOnlyWhenEnabled(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "setting.json", ToolDefinitionPaths: []string{sampleLoadToolManifest}}
	catalog, err := BuildCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Definitions()) != 0 {
		t.Fatalf("definitions without load_tool = %v, want none", catalog.Definitions())
	}
	config.NativeLoadTool = &sessionfs.NativeLoadToolSettings{}
	catalog, err = BuildCatalog(config)
	if err != nil {
		t.Fatalf("load_tool enabled: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != loadToolID || definitions[0].Name != "load_tool" {
		t.Fatalf("definitions = %v, want load_tool", definitions)
	}
}

// load_tool enabled with no lazy list is listed in the array and lists nothing: a profile that
// enables the tool without a lazy list is not refused, and the key alone decides whether it is listed.
func TestLoadToolWithNoLazySetListsNothing(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "setting.json", ToolDefinitionPaths: []string{sampleLoadToolManifest}, NativeLoadTool: &sessionfs.NativeLoadToolSettings{}}
	catalog, err := BuildCatalog(config)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(catalog.Definitions()) != 1 {
		t.Fatalf("definitions = %v, want load_tool", catalog.Definitions())
	}
	// load_tool is in the tools array sent to the model, although nothing is lazy.
	array, err := catalog.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(array) != 1 || !strings.Contains(string(array[0]), `"name":"load_tool"`) {
		t.Fatalf("tools array = %s, want load_tool listed", array)
	}
	// The list call with no names runs against the empty set the profile has.
	tool, err := NewLoadToolTool(nil, 16)
	if err != nil {
		t.Fatal(err)
	}
	text := runLoadTool(t, tool, map[string]any{}, tools.ExecutionContext{CallID: "call-1"})
	if text != "No lazy tools." {
		t.Fatalf("list = %q, want %q", text, "No lazy tools.")
	}
}

// A missing definition for an enabled load_tool is a configuration error that names it.
func TestLoadToolNeedsItsDefinition(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "setting.json", NativeLoadTool: &sessionfs.NativeLoadToolSettings{}}
	if _, err := BuildCatalog(config); err == nil || !strings.Contains(err.Error(), "native.load_tool is missing from tool_definition_paths") {
		t.Fatalf("err = %v, want the missing-definition error", err)
	}
}
