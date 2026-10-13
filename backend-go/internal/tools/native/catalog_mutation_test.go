package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestBuildCatalogLoadsConfiguredWriteAndEditToolsInRegistryOrder(t *testing.T) {
	manifest := writeMutationManifest(t, true, true, false)
	writeSettings := testWriteSettings(t.TempDir(), 1024)
	editSettings := testEditSettings(t.TempDir(), 512)
	catalog, err := BuildCatalogWithLocks(sessionfs.SessionConfig{
		SettingPath:         "/session/setting.json",
		ToolDefinitionPaths: []string{manifest},
		NativeWrite:         &writeSettings,
		NativeEdit:          &editSettings,
	}, NewPathLocks())
	if err != nil {
		t.Fatalf("BuildCatalogWithLocks() error = %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 2 || definitions[0].ID != writeToolID || definitions[1].ID != editToolID {
		t.Fatalf("definitions order = %#v", definitions)
	}
}

func TestBuildCatalogRejectsMissingOrTamperedMutationDefinitions(t *testing.T) {
	writeSettings := testWriteSettings(t.TempDir(), 1024)
	editSettings := testEditSettings(t.TempDir(), 512)

	missing := writeMutationManifest(t, true, false, false)
	_, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         "/session/setting.json",
		ToolDefinitionPaths: []string{missing},
		NativeWrite:         &writeSettings,
		NativeEdit:          &editSettings,
	})
	if err == nil || !strings.Contains(err.Error(), "native.edit is missing from tool_definition_paths") {
		t.Fatalf("missing edit definition error = %v", err)
	}

	tampered := writeMutationManifest(t, true, true, true)
	_, err = BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         "/session/setting.json",
		ToolDefinitionPaths: []string{tampered},
		NativeWrite:         &writeSettings,
	})
	if err == nil || !strings.Contains(err.Error(), "native.write inputSchema must exactly match") {
		t.Fatalf("tampered write definition error = %v", err)
	}
}

func writeMutationManifest(t *testing.T, includeWrite, includeEdit, tamperWrite bool) string {
	t.Helper()
	definitions := make([]tools.Definition, 0, 2)
	if includeWrite {
		schema := append(json.RawMessage(nil), writeInputSchema...)
		if tamperWrite {
			schema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)
		}
		definitions = append(definitions, tools.Definition{
			ID: "native.write", Source: "native", Name: "write", Description: "write", InputSchema: schema,
		})
	}
	if includeEdit {
		definitions = append(definitions, tools.Definition{
			ID: "native.edit", Source: "native", Name: "edit", Description: "edit", InputSchema: append(json.RawMessage(nil), editInputSchema...),
		})
	}
	data, err := json.Marshal(struct {
		SchemaVersion int                `json:"schema_version"`
		Tools         []tools.Definition `json:"tools"`
	}{SchemaVersion: 1, Tools: definitions})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
