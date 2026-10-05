package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

func TestNativeMemoryDefinitionMatchesTypeScriptFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "native-memory-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected tools.Definition
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	tool, err := NewMemoryTool(memoryTestSettings(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Definition.ID != expected.ID || tool.Definition.Source != expected.Source || tool.Definition.Name != expected.Name || tool.Definition.Description != expected.Description {
		t.Fatalf("Go native.memory definition = %+v, TypeScript fixture = %+v", tool.Definition, expected)
	}
	matches, err := sameJSON(tool.Definition.InputSchema, expected.InputSchema)
	if err != nil || !matches {
		t.Fatalf("Go native.memory inputSchema matches TypeScript = %v, error = %v", matches, err)
	}
}

func TestBuildCatalogLoadsConfiguredNativeMemory(t *testing.T) {
	directory := t.TempDir()
	manifest := writeNativeMemoryManifest(t, directory)
	settings := memoryTestSettings(filepath.Join(directory, "memory"))
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{manifest}, NativeMemory: &settings,
	})
	if err != nil {
		t.Fatalf("build memory catalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != memoryToolID || definitions[0].Name != "memory" {
		t.Fatalf("memory catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogRequiresExactNativeMemoryManifest(t *testing.T) {
	directory := t.TempDir()
	settings := memoryTestSettings(filepath.Join(directory, "memory"))
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeMemory: &settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("missing memory manifest error = %v", err)
	}
	manifest := writeNativeMemoryManifest(t, directory)
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
	badManifest := filepath.Join(directory, "bad-memory.json")
	if err := os.WriteFile(badManifest, altered, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{badManifest}, NativeMemory: &settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("mismatched memory schema error = %v", err)
	}
}

func writeNativeMemoryManifest(t *testing.T, directory string) string {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("testdata", "native-memory-definition.json"))
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
	path := filepath.Join(directory, "memory-manifest.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
