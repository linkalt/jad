package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func makeGrepManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewGrepTool(sessionfs.NativeGrepSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary: "rg", MaxResults: 200, MaxMatchesPerFile: 20, MaxBytes: 64 * 1024,
		MaxLineChars: 240, MaxFileBytes: 8 * 1024 * 1024, TimeoutMs: 10_000,
	})
	if err != nil {
		t.Fatalf("NewGrepTool() error = %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		pattern := properties["pattern"].(map[string]any)
		pattern["minLength"] = float64(2)
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
	path := filepath.Join(directory, "grep.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredNativeGrepAndMatchesManifest(t *testing.T) {
	directory := t.TempDir()
	manifest := makeGrepManifest(t, directory, false)
	settings := &sessionfs.NativeGrepSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Binary: "rg", MaxResults: 200, MaxMatchesPerFile: 20, MaxBytes: 64 * 1024,
		MaxLineChars: 240, MaxFileBytes: 8 * 1024 * 1024, TimeoutMs: 10_000,
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeGrep:          settings,
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != grepToolID || definitions[0].Name != "grep" {
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

func TestBuildCatalogRejectsMissingOrTamperedNativeGrepManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.NativeGrepSettings{Binary: "rg", MaxResults: 1, MaxMatchesPerFile: 1, MaxBytes: 1, MaxLineChars: 20, MaxFileBytes: 1, TimeoutMs: 1}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeGrep: settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("BuildCatalog(missing manifest) error = %v", err)
	}

	manifest := makeGrepManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeGrep:          settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("BuildCatalog(tampered schema) error = %v", err)
	}
}
