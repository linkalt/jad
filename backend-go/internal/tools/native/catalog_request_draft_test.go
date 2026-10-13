package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func makeRequestDraftManifest(t *testing.T, directory string, tamper bool) string {
	t.Helper()
	tool, err := NewRequestDraftTool(sessionfs.NativeRequestDraftSettings{
		TimeoutMS: 300_000, HeadlessAutoAcceptTimeoutMS: 600_000, MaxTimeoutMS: 1_800_000, ExtendMS: 300_000,
		MaxDraftBytes: 32 * 1024, MaxDraftDepth: 8, MaxTitleChars: 200,
	})
	if err != nil {
		t.Fatalf("NewRequestDraftTool: %v", err)
	}
	definition := tool.Definition
	if tamper {
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		draft := properties["draft"].(map[string]any)
		draft["additionalProperties"] = false
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
	path := filepath.Join(directory, "request-draft.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredRequestDraft(t *testing.T) {
	directory := t.TempDir()
	settings := sessionfs.NativeRequestDraftSettings{
		TimeoutMS: 300_000, HeadlessAutoAcceptTimeoutMS: 600_000, MaxTimeoutMS: 1_800_000, ExtendMS: 300_000,
		MaxDraftBytes: 32 * 1024, MaxDraftDepth: 8, MaxTitleChars: 200,
	}
	manifest := makeRequestDraftManifest(t, directory, false)
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeRequestDraft:  &settings,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != requestDraftToolID || definitions[0].Name != "request_draft" {
		t.Fatalf("request_draft catalog definitions = %#v", definitions)
	}
	if !catalog.IsUserDecisionCall(json.RawMessage(`{"name":"request_draft"}`)) {
		t.Fatal("request_draft was not marked as requiring a user decision")
	}
}

func TestBuildCatalogRequiresExactRequestDraftManifest(t *testing.T) {
	directory := t.TempDir()
	settings := sessionfs.NativeRequestDraftSettings{
		TimeoutMS: 300_000, HeadlessAutoAcceptTimeoutMS: 600_000, MaxTimeoutMS: 1_800_000, ExtendMS: 300_000,
		MaxDraftBytes: 32 * 1024, MaxDraftDepth: 8, MaxTitleChars: 200,
	}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeRequestDraft: &settings})
	if err == nil || !strings.Contains(err.Error(), "missing from tool_definition_paths") {
		t.Fatalf("missing request_draft manifest error = %v", err)
	}
	manifest := makeRequestDraftManifest(t, directory, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{manifest},
		NativeRequestDraft:  &settings,
	})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("tampered request_draft schema error = %v", err)
	}
}
