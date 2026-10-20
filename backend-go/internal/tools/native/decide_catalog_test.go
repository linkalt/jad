package native

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/extension"
)

func TestBuildCatalogLoadsConfiguredDecideAndRequiresItsManifest(t *testing.T) {
	directory := t.TempDir()
	settings := &sessionfs.ExtensionDecideSettings{Backend: "openai", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "K", Model: "m", TimeoutMS: 1000, MaxQuestions: 1, MaxInputBytes: 10, MaxResponseBytes: 10}
	_, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ExtensionDecide: settings})
	if err == nil || !strings.Contains(err.Error(), "tools.extension.decide is configured but extension.decide is missing from tool_definition_paths") {
		t.Fatalf("missing manifest error = %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "extension", "decide.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "decide.json")
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{path}, ExtensionDecide: settings})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	definitions := catalog.Definitions()
	if len(definitions) != 1 || definitions[0].ID != extension.DecideToolID {
		t.Fatalf("definitions = %#v", definitions)
	}
	tampered := strings.Replace(string(manifest), `"minItems": 1`, `"minItems": 2`, 1)
	if tampered == string(manifest) {
		t.Fatal("the tamper edit did not change the manifest")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = BuildCatalog(sessionfs.SessionConfig{SettingPath: filepath.Join(directory, "setting.json"), ToolDefinitionPaths: []string{path}, ExtensionDecide: settings})
	if err == nil || !strings.Contains(err.Error(), "inputSchema must exactly match") {
		t.Fatalf("tampered manifest error = %v", err)
	}
	empty, err := BuildCatalog(sessionfs.SessionConfig{SettingPath: "setting.json"})
	if err != nil || len(empty.Definitions()) != 0 {
		t.Fatalf("decide appeared without being configured: %v", err)
	}
}

func TestSubagentCanListDecideBecauseItIsReadOnly(t *testing.T) {
	tool, err := extension.NewDecideTool(sessionfs.ExtensionDecideSettings{}, extension.DecideImages{})
	if err != nil {
		t.Fatal(err)
	}
	agent := sessionfs.NativeSubagentAgent{Name: "triager", HasTools: true, Tools: []string{"decide"}}
	chosen, _, err := subagentChildTools(agent, []tools.Tool{tool}, nil, false)
	if err != nil || len(chosen) != 1 || chosen[0].Definition.Name != "decide" {
		t.Fatalf("chosen = %v, err %v", chosen, err)
	}
}

// BuildCatalog must give decide its own path policy and the session's image store, and
// must not lend it the read tool's policy: read's deny rule must not apply to decide.
func TestBuildCatalogWiresOwnPolicyAndImageStoreIntoDecide(t *testing.T) {
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openAIReply))
	}))
	t.Cleanup(server.Close)

	sample := filepath.Join("..", "..", "..", "..", "sample", "tools")
	storeDir := filepath.Join(t.TempDir(), "images")
	store := imagestore.New(imagestore.Options{Dir: storeDir, MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24})
	data := decidePNG(t, 8, 8)
	stored, _ := store.Externalize([]byte(`{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(data) + `"}]}`))
	names := imagestore.RefNames(stored)
	if len(names) != 1 {
		t.Fatalf("setup: %v", names)
	}
	for _, name := range []string{"private/a.png", "public/a.png"} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := BuildCatalog(sessionfs.SessionConfig{
		SettingPath:         filepath.Join(workspace, "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join(sample, "extension", "decide.json"), filepath.Join(sample, "native", "read.json")},
		NativeRead:          &sessionfs.NativeReadSettings{Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "public")}}}, MaxBytes: 1000, MaxFileBytes: 1000, DefaultLimit: 10},
		ExtensionDecide:     &sessionfs.ExtensionDecideSettings{Backend: "openai", BaseURL: server.URL, APIKeyEnv: "PATH", Model: "m", TimeoutMS: 5000, MaxQuestions: 4, MaxInputBytes: 1000, MaxResponseBytes: 4096, MaxImages: 2, MaxImageBytes: 4000, Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "deny", Path: filepath.Join(workspace, "private")}}}},
		ImageStore:          sessionfs.ImageStoreSettings{Dir: storeDir, MinBytes: 1, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) string {
		arguments := `{"input":[{"type":"image","path":` + quote(path) + `}],` + imageQuestion + `}`
		raw, err := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": "decide", "arguments": arguments})
		if err != nil {
			t.Fatal(err)
		}
		result, err := catalog.ExecuteFunctionCall(context.Background(), raw, tools.ExecutionContext{WorkspaceRoot: workspace})
		if err != nil {
			t.Fatal(err)
		}
		return string(result)
	}
	if output := call("private/a.png"); !strings.Contains(output, "path_denied") || received != 0 {
		t.Fatalf("a path decide denies was read (requests %d):\n%s", received, output)
	}
	if output := call("public/a.png"); !strings.Contains(output, `status\":\"ok`) && !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("a path only the read tool denies was refused for decide:\n%s", output)
	}
	if output := call("stored-image://" + names[0]); !strings.Contains(output, `status\":\"ok`) && !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("a stored image was refused:\n%s", output)
	}
	if received != 2 {
		t.Fatalf("%d requests, want 2", received)
	}
}
