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

func TestHTTPCompactPersistsProcessAndTaskStateContextAndInjectsItIntoNextRequest(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "one")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	processStorage := filepath.Join(sessionDir, "processes")
	processTool, err := native.NewProcessTool(sessionfs.NativeProcessSettings{
		StorageDir: processStorage,
		Shell:      "/bin/sh",
	}, nil, "UNSET_PROVIDER_KEY")
	if err != nil {
		t.Fatal(err)
	}
	taskStorage := filepath.Join(sessionDir, "tasks")
	taskSettings := sessionfs.NativeTaskSettings{
		MaxTasks: 10, MaxStorageBytes: 64 * 1024,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress"}, Limit: 5},
	}
	taskStore := native.NewTaskStore(taskStorage)
	taskTool, err := native.NewTaskTool(taskSettings, taskStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskStore.Create(native.TaskInput{
		Title: "remember the active task", Status: native.TaskInProgress, Priority: native.TaskPriorityHigh,
		Tags: []string{"porting"}, Documents: []string{"plan.md"},
	}, taskSettings); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{processTool.Definition, taskTool.Definition}})
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
	var requestIncludedStateContext atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-provider-key" {
			http.Error(w, "unexpected provider credentials", http.StatusUnauthorized)
			return
		}
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		providerCalls.Add(1)
		for _, item := range request.Input {
			var message struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(item, &message) != nil || message.Role != "developer" {
				continue
			}
			for _, content := range message.Content {
				if strings.Contains(content.Text, "STATE CONTEXT SNAPSHOT") && strings.Contains(content.Text, "proc_saved") && strings.Contains(content.Text, "T-001") {
					requestIncludedStateContext.Store(true)
				}
			}
		}
		events := []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-after-compact","status":"completed","model":"gpt-test","output":[]}}`,
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

	rootSetting := `{"session_roots":[{"id":"local","path":"sessions"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(rootSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":2,"tool_definition_paths":["process.json"],"compaction":{"keepbatch":{"turn":{"type":"assistant_final","keep":1}}},"statecontext":{"enabled":true,"trigger":["compaction"],"tools":["process","task"],"role":"developer","max_bytes":4096},"tools":{"native":{"process":{"shell":"/bin/sh","storage_dir":%q,"statecontext":{"states":["exited"],"limit":5}},"task":{"storage_dir":%q,"statecontext":{"statuses":["in-progress"],"limit":5}}}}}`, workspace, processStorage, taskStorage)
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

	compactRequest, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:one/compact", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	compactRequest.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	compactRequest.Header.Set("Content-Type", "application/json")
	compactResponse, err := http.DefaultClient.Do(compactRequest)
	if err != nil {
		t.Fatal(err)
	}
	compactBody, err := io.ReadAll(compactResponse.Body)
	compactResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if compactResponse.StatusCode != http.StatusOK || !strings.Contains(string(compactBody), `"compacted":true`) {
		t.Fatalf("compact response status=%d body=%s", compactResponse.StatusCode, compactBody)
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
	if snapshot.SchemaVersion != 1 || snapshot.Trigger != "compaction" || !strings.Contains(snapshot.Text, "proc_saved") || !strings.Contains(snapshot.Text, "T-001") {
		t.Fatalf("persisted state context snapshot = %#v", snapshot)
	}

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
	if runResponse.StatusCode != http.StatusOK || !strings.Contains(string(runBody), "continued") {
		t.Fatalf("run response status=%d body=%s", runResponse.StatusCode, runBody)
	}
	if providerCalls.Load() != 1 || !requestIncludedStateContext.Load() {
		t.Fatalf("provider calls=%d, state context present=%v", providerCalls.Load(), requestIncludedStateContext.Load())
	}
	transcriptBytes, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcriptBytes), "STATE CONTEXT SNAPSHOT") || strings.Contains(string(transcriptBytes), "proc_saved") || strings.Contains(string(transcriptBytes), "T-001") {
		t.Fatalf("derived state context was written into messages.jsonl: %s", transcriptBytes)
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(transcriptBytes)))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("messages.jsonl after compact and run has %d lines, want only new user and response records", len(lines))
	}
}

func writeProcessManifest(t *testing.T, storageDir string, manifest native.ProcessManifest) {
	t.Helper()
	processDir := filepath.Join(storageDir, manifest.ID)
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
