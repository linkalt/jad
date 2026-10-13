package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func makeTaskManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewTaskTool(taskTestSettings(filepath.Join(directory, "tasks")), nil)
	if err != nil {
		t.Fatalf("NewTaskTool: %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		limit := properties["limit"].(map[string]any)
		limit["minimum"] = float64(2)
		definition.InputSchema, err = json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(struct {
		SchemaVersion int `json:"schema_version"`
		Tools         any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{definition}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "task.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredNativeTask(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	manifest := makeTaskManifest(t, directory, false)
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeTask:          &settings,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != taskToolID || definitions[0].Name != "task" {
		t.Fatalf("task catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogRequiresExactNativeTaskManifest(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeTask: &settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("missing task manifest error = %v", err)
	}

	manifest := makeTaskManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeTask:          &settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("tampered task schema error = %v", err)
	}
}

func TestNativeTaskDefinitionMatchesTypeScriptFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "native-task-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var typescriptDefinition struct {
		ID          string          `json:"id"`
		Source      string          `json:"source"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(data, &typescriptDefinition); err != nil {
		t.Fatal(err)
	}
	tool, err := NewTaskTool(taskTestSettings(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	definition := tool.Definition
	if definition.ID != typescriptDefinition.ID || definition.Source != typescriptDefinition.Source || definition.Name != typescriptDefinition.Name || definition.Description != typescriptDefinition.Description {
		t.Fatalf("Go native.task definition = %+v, TypeScript definition = %+v", definition, typescriptDefinition)
	}
	matches, err := sameJSON(definition.InputSchema, typescriptDefinition.InputSchema)
	if err != nil || !matches {
		t.Fatalf("Go native.task inputSchema matches TypeScript = %v, error = %v", matches, err)
	}
}
