package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func makeBashManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewBashTool(sessionfs.NativeBashSettings{}, "")
	if err != nil {
		t.Fatalf("NewBashTool() error = %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		command := properties["command"].(map[string]any)
		command["minLength"] = float64(2)
		definition.InputSchema, err = json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{definition}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "bash.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredNativeBashAndMatchesManifest(t *testing.T) {
	directory := t.TempDir()
	manifest := makeBashManifest(t, directory, false)
	settings := &sessionfs.NativeBashSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:  "/bin/bash", Env: map[string]string{}, EnvDeny: []string{},
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeBash:          settings,
		Model:               sessionfs.Model{APIKeyEnv: "FIXTURE_OPENAI_KEY"},
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != bashToolID || definitions[0].Name != "bash" {
		t.Fatalf("catalog definitions = %#v", definitions)
	}
	responseTools, err := catalog.ResponseTools()
	if err != nil || len(responseTools) != 1 || !strings.Contains(string(responseTools[0]), `"name":"bash"`) {
		t.Fatalf("ResponseTools() = %s, error = %v", responseTools, err)
	}

	empty, err := BuildCatalog(sessionfs.SessionConfig{})
	if err != nil {
		t.Fatalf("BuildCatalog(disabled) error = %v", err)
	}
	if len(empty.Definitions()) != 0 {
		t.Fatalf("disabled catalog definitions = %#v", empty.Definitions())
	}
}

func TestBuildCatalogMatchesTypeScriptNativeBashManifest(t *testing.T) {
	manifest := filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "bash.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("stat TypeScript bash manifest %s: %v", manifest, err)
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeBash:          &sessionfs.NativeBashSettings{Shell: "/bin/bash", Env: map[string]string{}},
	})
	if err != nil {
		t.Fatalf("BuildCatalog(TypeScript bash manifest) error = %v", err)
	}
	if definitions := catalog.Definitions(); len(definitions) != 1 || definitions[0].ID != bashToolID {
		t.Fatalf("catalog definitions = %#v", definitions)
	}
}

func TestBuildCatalogRejectsMissingOrTamperedNativeBashManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.NativeBashSettings{Shell: "/bin/bash", Env: map[string]string{}}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeBash: settings})
	if err == nil || !strings.Contains(err.Error(), "tools.native.bash is configured but native.bash is missing") {
		t.Fatalf("BuildCatalog(missing manifest) error = %v", err)
	}

	manifest := makeBashManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeBash:          settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("BuildCatalog(tampered schema) error = %v", err)
	}
}
