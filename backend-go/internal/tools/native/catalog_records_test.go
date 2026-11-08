package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestNativeRecordsDefinitionMatchesTypeScriptFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "native-records-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected tools.Definition
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	tool, err := NewRecordsTool(recordsTestSettings(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Definition.ID != expected.ID || tool.Definition.Source != expected.Source || tool.Definition.Name != expected.Name || tool.Definition.Description != expected.Description {
		t.Fatalf("Go native.records definition = %+v, TypeScript fixture = %+v", tool.Definition, expected)
	}
	matches, err := sameJSON(tool.Definition.InputSchema, expected.InputSchema)
	if err != nil || !matches {
		t.Fatalf("Go native.records inputSchema matches TypeScript = %v, error = %v", matches, err)
	}
}

func TestBuildCatalogLoadsConfiguredNativeRecords(t *testing.T) {
	directory := t.TempDir()
	manifest := writeNativeRecordsManifest(t, directory)
	settings := recordsTestSettings(filepath.Join(directory, "records"))
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{manifest}, NativeRecords: &settings,
	})
	if err != nil {
		t.Fatalf("build records catalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != recordsToolID || definitions[0].Name != "records" {
		t.Fatalf("records catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogRequiresExactNativeRecordsManifest(t *testing.T) {
	directory := t.TempDir()
	settings := recordsTestSettings(filepath.Join(directory, "records"))
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeRecords: &settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("missing records manifest error = %v", err)
	}
	manifest := writeNativeRecordsManifest(t, directory)
	var document struct {
		Tools []json.RawMessage `json:"tools"`
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var definition map[string]any
	if err := json.Unmarshal(document.Tools[0], &definition); err != nil {
		t.Fatal(err)
	}
	definition["inputSchema"] = map[string]any{"type": "object"}
	altered, err := json.Marshal(map[string]any{"tools": []any{definition}})
	if err != nil {
		t.Fatal(err)
	}
	badManifest := filepath.Join(directory, "bad-records.json")
	if err := os.WriteFile(badManifest, altered, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{badManifest}, NativeRecords: &settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") || !strings.Contains(err.Error(), "replaced with sample/tools/native/records.json") {
		t.Fatalf("mismatched records schema error = %v", err)
	}
}

func writeNativeRecordsManifest(t *testing.T, directory string) string {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("testdata", "native-records-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		SchemaVersion int               `json:"schema_version"`
		Tools         []json.RawMessage `json:"tools"`
	}{SchemaVersion: 1, Tools: []json.RawMessage{definition}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "records-manifest.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecordsToolIsNotReachableByItsOldName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	tool, err := NewRecordsTool(recordsTestSettings(root), NewRecordsStore(root))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call-old", "name": "memory", "arguments": `{"operation":"list","type":"note"}`})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "unknown_tool") {
		t.Fatalf("call by the old name memory = %s, want unknown_tool", raw)
	}
}
