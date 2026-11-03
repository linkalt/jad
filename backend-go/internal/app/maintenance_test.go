package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestHTTPCompactAndResetWithFilesystemResolver(t *testing.T) {
	config, _ := layoutConfig(t, maintenanceSpec(map[string]any{
		"compaction":  map[string]any{"keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
		"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576, "response_metadata": "full"},
	}, ""), map[string]string{"JAD_API_TOKEN": integrationAPIToken})
	location, err := sessionfs.Resolve(config.Roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	messagesPath := sessionConfig.MessagesPath
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(messagesPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	api := httptest.NewServer(service.Handler)
	defer api.Close()

	compact := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	if compact.StatusCode != http.StatusOK {
		body := readMutationResponse(t, compact)
		t.Fatalf("compact status=%d body=%s", compact.StatusCode, body)
	}
	var result struct {
		Compaction struct {
			Compacted      bool `json:"compacted"`
			CompactedItems int  `json:"compacted_items"`
			RetainedItems  int  `json:"retained_items"`
		} `json:"compaction"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.NewDecoder(compact.Body).Decode(&result); err != nil {
		compact.Body.Close()
		t.Fatal(err)
	}
	compact.Body.Close()
	if !result.Compaction.Compacted || result.Compaction.CompactedItems != 4 || result.Compaction.RetainedItems != 2 || len(result.Usage) == 0 {
		t.Fatalf("compact response = %+v", result)
	}
	if contents, err := os.ReadFile(messagesPath); err != nil || len(contents) != 0 {
		t.Fatalf("messages after compact = %q, err=%v", contents, err)
	}
	cachePath := filepath.Join(sessionConfig.StagingDir, "compaction-cache.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("compaction cache missing: %v", err)
	}
	shared, _, err := service.resolver.sharedStore(sessionConfig)
	if err != nil {
		t.Fatal(err)
	}
	history, err := shared.store.ReadInputItems()
	if err != nil || len(history) != 2 || !strings.Contains(string(history[0]), "latest context") || !strings.Contains(string(history[1]), "latest answer") {
		t.Fatalf("history after compact = %q, err=%v", history, err)
	}

	reset := postEmptyMutation(t, api.URL+"/api/sessions/local:one/reset", integrationAPIToken)
	if reset.StatusCode != http.StatusOK {
		body := readMutationResponse(t, reset)
		t.Fatalf("reset status=%d body=%s", reset.StatusCode, body)
	}
	if body := readMutationResponse(t, reset); !strings.Contains(body, `"reset":true`) {
		t.Fatalf("reset response = %s", body)
	}
	for _, path := range []string{filepath.Join(filepath.Dir(messagesPath), "messages.backup.jsonl"), cachePath, cachePath + ".backup"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset left %s: %v", path, err)
		}
	}
}

// TypeScript opens a session for reset and front-compact without reading
// model.json, but compact() loads the full configuration. A broken model file
// must therefore block compact (before the transcript changes) and nothing else.
func TestMaintenanceOnlyCompactNeedsAUsableModelFile(t *testing.T) {
	config, _ := layoutConfig(t, maintenanceSpec(map[string]any{
		"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576, "response_metadata": "full"},
	}, `{"provider":"acme","api_key_env":"not a valid name"}`), map[string]string{"JAD_API_TOKEN": integrationAPIToken})
	location, err := sessionfs.Resolve(config.Roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	sessionSettings, err := sessionfs.LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	messagesPath := sessionSettings.MessagesPath
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}` + "\n"
	if err := os.WriteFile(messagesPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	api := httptest.NewServer(service.Handler)
	defer api.Close()

	compact := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	if body := readMutationResponse(t, compact); compact.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "model must be a non-empty string") {
		t.Fatalf("compact status=%d body=%s", compact.StatusCode, body)
	}
	if contents, err := os.ReadFile(messagesPath); err != nil || string(contents) != transcript {
		t.Fatalf("compact changed the transcript: %q, err=%v", contents, err)
	}
	reset := postEmptyMutation(t, api.URL+"/api/sessions/local:one/reset", integrationAPIToken)
	if body := readMutationResponse(t, reset); reset.StatusCode != http.StatusOK || !strings.Contains(body, `"reset":true`) {
		t.Fatalf("reset status=%d body=%s", reset.StatusCode, body)
	}
}

// maintenanceSpec is the session "one" the maintenance tests post to; modelRaw, when set,
// is the exact text of its model file.
func maintenanceSpec(settings map[string]any, modelRaw string) layouttest.Spec {
	spec := layouttest.Spec{
		SessionID: "one",
		Settings:  settings,
		Server:    map[string]any{"max_body_bytes": 4096, "sse_max_buffered_bytes": 4096},
		ModelRaw:  modelRaw,
	}
	if modelRaw == "" {
		spec.Models = map[string]map[string]any{"openai/default": {"provider": "openai", "model": "offline-fixture", "api_key_env": "UNSET_PROVIDER_KEY"}}
	}
	return spec
}
