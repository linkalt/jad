package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func makeReadImageManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewReadImageTool(sessionfs.NativeReadImageSettings{MaxImageBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("NewReadImageTool() error = %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		pathSchema := properties["path"].(map[string]any)
		pathSchema["minLength"] = float64(2)
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
	path := filepath.Join(directory, "read_image.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsOnlyConfiguredNativeReadImageAndMatchesManifest(t *testing.T) {
	directory := t.TempDir()
	manifest := makeReadImageManifest(t, directory, false)
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeReadImage: &sessionfs.NativeReadImageSettings{
			Policy:        sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
			MaxImageBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatalf("BuildCatalog() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != readImageToolID || definitions[0].Name != "read_image" {
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

func TestBuildCatalogRejectsMissingOrTamperedReadImageManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.NativeReadImageSettings{MaxImageBytes: 1 << 20}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeReadImage: settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("BuildCatalog(missing manifest) error = %v", err)
	}

	manifest := makeReadImageManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeReadImage:     settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("BuildCatalog(tampered schema) error = %v", err)
	}
}
