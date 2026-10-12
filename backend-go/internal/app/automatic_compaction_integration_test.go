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
	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func TestHTTPRunAutomaticallyCompactsAndPersistsStateContext(t *testing.T) {
	workspace := t.TempDir()
	processStorage := filepath.Join(workspace, "processes")
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

	writeProcessManifest(t, processStorage, native.ProcessManifest{
		SchemaVersion: 1,
		ID:            "proc_saved",
		State:         native.ProcessExited,
		CWD:           "/workspace/project",
		StartedAt:     "2026-10-02T10:00:00.000Z",
		EndedAt:       "2026-10-02T10:00:01.000Z",
		ExitCode:      json.RawMessage("0"),
		Signal:        json.RawMessage("null"),
	})

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

	config, fixture := layoutConfig(t, layouttest.Spec{
		SessionID:           "one",
		CWD:                 workspace,
		ToolDefinitionPaths: []string{filepath.Join(workspace, "process.json")},
		Settings: map[string]any{
			"max_tool_rounds": 1,
			"compaction":      map[string]any{"auto_enabled": true, "trigger_tokens": 1, "keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}},
			"statecontext":    map[string]any{"enabled": true, "trigger": []string{"compaction"}, "tools": []string{"process"}, "role": "developer", "max_bytes": 4096},
		},
		Native: map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": processStorage, "statecontext": map[string]any{"states": []string{"exited"}, "limit": 5}}},
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	messages := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}]}}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest context"}]}`,
		`{"type":"response.completed","response":{"id":"resp_latest","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"latest answer"}]}]}}`,
	}, "\n") + "\n"
	messagesPath := filepath.Join(profileState(fixture), "messages.jsonl")
	if err := os.MkdirAll(filepath.Dir(messagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messagesPath, []byte(messages), 0o600); err != nil {
		t.Fatal(err)
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
