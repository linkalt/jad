package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func TestHTTPRunAutomaticallyCompactsAndPersistsStateContext(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "one")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	processStorage := filepath.Join(sessionDir, "processes")
	processTool, err := native.NewProcessTool(sessionfs.NativeProcessSettings{StorageDir: processStorage, Shell: "/bin/sh"}, nil, "UNSET_PROVIDER_KEY")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{processTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "process.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-provider-key" {
			http.Error(w, "unexpected provider credentials", http.StatusUnauthorized)
			return
		}
		providerCalls.Add(1)
		events := []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after automatic compaction"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-auto-compact","status":"completed","model":"gpt-test","usage":{"input_tokens":100},"output":[]}}`,
			"[DONE]",
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":1,"tool_definition_paths":["process.json"],"compaction":{"auto_enabled":true,"trigger_tokens":1,"keepbatch":{"turn":{"type":"assistant_final","keep":1}}},"statecontext":{"enabled":true,"trigger":["compaction"],"tools":["process"],"role":"developer","max_bytes":4096},"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"statecontext":{"states":["exited"],"limit":5}}}}}`, workspace, processStorage)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(sessionSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	modelSetting := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(modelSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_latest","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}`,
	}, "\n") + "\n"
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	if err := os.WriteFile(messagesPath, []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: workspace,
		Environment: map[string]string{
			"AGENT_API_TOKEN":    integrationAPIToken,
			"FIXTURE_OPENAI_KEY": "local-provider-key",
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	runRequest, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:one/run", strings.NewReader(`{"input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	runRequest.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	runRequest.Header.Set("Content-Type", "application/json")
	runResponse, err := http.DefaultClient.Do(runRequest)
	if err != nil {
		t.Fatal(err)
	}
	runBody, err := io.ReadAll(runResponse.Body)
	runResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if runResponse.StatusCode != http.StatusOK || !strings.Contains(string(runBody), "after automatic compaction") {
		t.Fatalf("run response status=%d body=%s", runResponse.StatusCode, runBody)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls=%d, want 1", providerCalls.Load())
	}

	location, err := sessionfs.Resolve(config.Roots, "local:one")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshotBytes, err := os.ReadFile(sessionConfig.StoragePaths.StateContext)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot transcript.StateContextSnapshot
	if err := json.Unmarshal(snapshotBytes, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != 1 || snapshot.Trigger != "compaction" || !strings.Contains(snapshot.Text, "STATE CONTEXT SNAPSHOT") {
		t.Fatalf("persisted state context snapshot = %#v", snapshot)
	}
	transcriptBytes, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcriptBytes), "STATE CONTEXT SNAPSHOT") || strings.Contains(string(transcriptBytes), "old context") {
		t.Fatalf("automatic compaction left old or derived state in messages.jsonl: %s", transcriptBytes)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(transcriptBytes)))
	if scanner.Scan() {
		t.Fatalf("messages.jsonl after completed-response compaction should be empty, got %q", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
