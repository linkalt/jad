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

func writeTaskDefinition(t *testing.T, directory, description string, schema json.RawMessage) string {
	t.Helper()
	data, err := json.Marshal(struct {
		SchemaVersion int `json:"schema_version"`
		Tools         any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{map[string]any{"id": taskToolID, "source": "native", "name": "task", "description": description, "inputSchema": schema}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "task-old.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogAcceptsTheTaskManifestBeforeOps(t *testing.T) {
	directory := t.TempDir()
	settings := taskTestSettings(filepath.Join(directory, "tasks"))
	if same, err := sameJSON(taskInputSchemaV1, taskInputSchema); err != nil || same {
		t.Fatalf("the frozen schema must differ from the current one: same = %v, error = %v", same, err)
	}
	for name, description := range map[string]string{"old description": taskDescriptionV1, "custom description": "Our own task text."} {
		manifest := writeTaskDefinition(t, directory, description, taskInputSchemaV1)
		catalog, err := BuildCatalog(sessionfs.SessionConfig{
			SettingPath:         filepath.Join(directory, "setting.json"),
			ToolDefinitionPaths: []string{manifest},
			NativeTask:          &settings,
		})
		if err != nil {
			t.Fatalf("%s: BuildCatalog with the old task manifest: %v", name, err)
		}
		definitions := catalog.Definitions()
		if len(definitions) != 1 {
			t.Fatalf("%s: definitions = %#v", name, definitions)
		}
		if same, err := sameJSON(definitions[0].InputSchema, taskInputSchema); err != nil || !same {
			t.Fatalf("%s: the model is not shown the current schema (%v, %v):\n%s", name, same, err, definitions[0].InputSchema)
		}
		wantDescription := description
		if description == taskDescriptionV1 {
			wantDescription = taskDescription
		}
		if definitions[0].Description != wantDescription {
			t.Fatalf("%s: description = %q, want %q", name, definitions[0].Description, wantDescription)
		}
	}
	// A schema that is neither version still fails with the replace-the-file hint.
	var schema map[string]any
	if err := json.Unmarshal(taskInputSchemaV1, &schema); err != nil {
		t.Fatal(err)
	}
	schema["properties"].(map[string]any)["limit"].(map[string]any)["minimum"] = float64(2)
	tampered, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{writeTaskDefinition(t, directory, taskDescriptionV1, tampered)},
		NativeTask:          &settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") || !strings.Contains(err.Error(), "sample/tools/native/task.json") {
		t.Fatalf("tampered old task schema error = %v", err)
	}
}

func TestSampleTaskManifestMatchesTheToolDefinition(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tools []struct {
			ID          string          `json:"id"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Tools) != 1 {
		t.Fatalf("decode the sample task manifest: %v", err)
	}
	if manifest.Tools[0].ID != taskToolID || manifest.Tools[0].Description != taskDescription {
		t.Fatalf("sample manifest = %+v", manifest.Tools[0])
	}
	if same, err := sameJSON(manifest.Tools[0].InputSchema, taskInputSchema); err != nil || !same {
		t.Fatalf("sample task manifest schema matches the code = %v, error = %v", same, err)
	}
}
