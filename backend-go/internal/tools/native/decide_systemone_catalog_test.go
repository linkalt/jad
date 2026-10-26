package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/extension"
)

func TestBuildCatalogLoadsConfiguredDecideSystemoneAndRequiresItsManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.ExtensionDecideSystemoneSettings{Backend: "openai", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "K", Model: "m", TimeoutMS: 1000, MaxQuestions: 1, MaxInputBytes: 10, MaxResponseBytes: 10}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ExtensionDecideSystemone: settings})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.decide_systemone is configured but extension.decide_systemone is missing from tool_definition_paths") {
		t.Fatalf("missing manifest error = %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "extension", "decide_systemone.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "decide_systemone.json")
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{path}, ExtensionDecideSystemone: settings})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != extension.DecideSystemoneToolID {
		t.Fatalf("definitions = %#v", definitions)
	}
	tampered := strings.Replace(string(manifest), `"minItems": 1`, `"minItems": 2`, 1)
	if tampered == string(manifest) {
		t.Fatal("the tamper edit did not change the manifest")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{path}, ExtensionDecideSystemone: settings})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("tampered manifest error = %v", err)
	}
	empty, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: "setting.json"})
	if err != nil || len(empty.Definitions()) != 0 {
		t.Fatalf("decide_systemone appeared without being configured: %v", err)
	}
}

func TestSubagentCanListDecideSystemoneBecauseItIsReadOnly(t *testing.T) {
	tool, err := extension.NewDecideSystemoneTool(sessionfs.ExtensionDecideSystemoneSettings{})
	if err != nil {
		t.Fatal(err)
	}
	agent := sessionfs.NativeSubagentAgent{Name: "triager", HasTools: true, Tools: []string{"decide_systemone"}}
	chosen, _, err := subagentChildTools(agent, []tools.Tool{tool}, nil)
	if err != nil || len(chosen) != 1 || chosen[0].Definition.Name != "decide_systemone" {
		t.Fatalf("chosen = %v, err %v", chosen, err)
	}
}

func TestSubagentRefusesTheOldDecideName(t *testing.T) {
	tool, err := extension.NewDecideSystemoneTool(sessionfs.ExtensionDecideSystemoneSettings{})
	if err != nil {
		t.Fatal(err)
	}
	agent := sessionfs.NativeSubagentAgent{Name: "triager", HasTools: true, Tools: []string{"decide"}}
	_, _, err = subagentChildTools(agent, []tools.Tool{tool}, nil)
	if err == nil || !strings.Contains(err.Error(), `lists "decide", which is not an enabled`) {
		t.Fatalf("old name error = %v", err)
	}
}
