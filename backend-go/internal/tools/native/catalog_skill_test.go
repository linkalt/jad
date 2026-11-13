package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func writeSkillManifest(t *testing.T, directory string) string {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("testdata", "native-skill-definition.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "skill-tools.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"tools":[`+string(definition)+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildCatalogLoadsConfiguredSkillAndServesIt(t *testing.T) {
	directory := t.TempDir()
	addSkill(t, filepath.Join(directory, ".agent", "skills"), "demo", "A demo skill.", "Demo body.")
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(directory, "setting.json"),
		ToolDefinitionPaths: []string{writeSkillManifest(t, directory)},
		NativeSkill:         &sessionfs.NativeSkillSettings{MaxBytes: 131072},
		Libraries:           &sessionfs.Libraries{GlobalDir: directory},
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != skillToolID || definitions[0].Name != "skill" {
		t.Fatalf("skill catalog definitions = %#v", definitions)
	}
	output := executeCatalogCall(t, catalog, "skill", `{"name":"Demo"}`)
	if !strings.Contains(output, "Demo body.") || !strings.Contains(output, "Loaded skill demo") {
		t.Fatalf("the catalog did not serve the skill:\n%s", output)
	}
}

func TestBuildCatalogRequiresTheSkillManifestAndItsExactSchema(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.NativeSkillSettings{MaxBytes: 131072}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), NativeSkill: settings, Libraries: &sessionfs.Libraries{GlobalDir: directory}})
	if err == nil || !strings.Contains(err.Error(), "tools.native.skill is configured but native.skill is missing from tool_definition_paths") {
		t.Fatalf("missing skill manifest error = %v", err)
	}
	tampered := filepath.Join(directory, "tampered.json")
	manifest := `{"schema_version":1,"tools":[{"id":"native.skill","source":"native","name":"skill","description":"d","inputSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}}]}`
	if err := os.WriteFile(tampered, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{tampered}, NativeSkill: settings, Libraries: &sessionfs.Libraries{GlobalDir: directory}})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("tampered skill schema error = %v", err)
	}
}

func TestBuildCatalogOmitsTheSkillToolWhenItIsNotConfigured(t *testing.T) {
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: "setting.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Definitions()) != 0 {
		t.Fatalf("an unconfigured catalog exposes %v", catalog.Definitions())
	}
	if output := executeCatalogCall(t, catalog, "skill", `{}`); !strings.Contains(output, "error_code: unknown_tool") {
		t.Fatalf("an unconfigured skill call = %s", output)
	}
}
