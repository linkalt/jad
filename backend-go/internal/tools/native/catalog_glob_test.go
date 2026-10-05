package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func makeGlobManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewGlobTool(sessionfs.NativeGlobSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary: "rg", IncludeHidden: true, MaxResults: 500, MaxBytes: 64 * 1024,
		MaxLineChars: 1000, TimeoutMs: 10_000,
	})
	if err != nil {
		t.Fatalf("NewGlobTool() error = %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		pattern := properties["pattern"].(map[string]any)
		pattern["minLength"] = float64(1)
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
	path := filepath.Join(directory, "glob.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredNativeGlobAndMatchesManifest(t *testing.T) {
	directory := t.TempDir()
	manifest := makeGlobManifest(t, directory, false)
	settings := &sessionfs.NativeGlobSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary: "rg", IncludeHidden: true, MaxResults: 500, MaxBytes: 64 * 1024,
		MaxLineChars: 1000, TimeoutMs: 10_000,
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeGlob:          settings,
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != globToolID || definitions[0].Name != "glob" {
		t.Fatalf("catalog definitions = %#v", definitions)
	}

	empty, err := BuildCatalog(sessionfs.SessionConfig{})
	if err != nil {
		t.Fatalf("BuildCatalog(disabled) error = %v", err)
	}
	if len(empty.Definitions()) != 0 {
		t.Fatalf("disabled catalog definitions = %#v", empty.Definitions())
	}
}

func TestBuildCatalogRejectsMissingOrTamperedNativeGlobManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.NativeGlobSettings{Binary: "rg", IncludeHidden: true, MaxResults: 1, MaxBytes: 1, MaxLineChars: 20, TimeoutMs: 1}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeGlob: settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("BuildCatalog(missing manifest) error = %v", err)
	}

	manifest := makeGlobManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeGlob:          settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("BuildCatalog(tampered schema) error = %v", err)
	}
}
