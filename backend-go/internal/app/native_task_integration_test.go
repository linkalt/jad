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
)

func TestHTTPRunExecutesNativeTaskAndPersistsOutputAcrossProviderRounds(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskStorage := filepath.Join(sessionDir, "tasks")
	taskSettings := sessionfs.NativeTaskSettings{
		MaxTasks: 20, ListLimit: 10, MaxListLimit: 20, MaxTitleChars: 100,
		MaxTags: 4, MaxTagChars: 20, MaxDocuments: 4, MaxDocumentChars: 100,
		MaxStorageBytes: 10_000, MaxListBytes: 1_000, StorageDir: taskStorage,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress", "done"}, Limit: 25},
	}
	taskTool, err := native.NewTaskTool(taskSettings, native.NewTaskStore(taskStorage))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{taskTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "task.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var secondRequestHadTaskOutput atomic.Bool
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
			Tools []json.RawMessage `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		var events []string
		switch providerCalls.Add(1) {
		case 1:
			if len(request.Tools) != 1 || !strings.Contains(string(request.Tools[0]), `"name":"task"`) {
				http.Error(w, "native task definition missing", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-task-add","name":"task","arguments":"{\"title\":\"Port the native task tool\",\"status\":\"in-progress\",\"priority\":\"high\",\"tags\":[\"Go\",\"go\"],\"documents\":[\"design_task.md\"]}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-task-add","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 2:
			output, found := findFunctionOutput(request.Input, "call-task-add")
			if !found || !strings.Contains(output, "task_id: T-001") || !strings.Contains(output, `tags: ["go"]`) {
				http.Error(w, "task output missing or incorrect in second request", http.StatusBadRequest)
				return
			}
			secondRequestHadTaskOutput.Store(true)
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"task saved"}]}}`,
				`{"type":"response.completed","response":{"id":"resp-task-final","status":"completed","model":"gpt-test","output":[]}}`,
			}
		default:
			http.Error(w, "unexpected extra provider round", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range append(events, "[DONE]") {
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
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":2,"tool_definition_paths":["task.json"],"tools":{"native":{"task":{"storage_dir":%q,"max_tasks":20}}}}`, workspace, taskStorage)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(sessionSetting), 0o600); err != nil {
		t.Fatal(err)
	}
	modelSetting := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(modelSetting), 0o600); err != nil {
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
		t.Fatalf("LoadConfig: %v", err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"add a task"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "task saved") {
		t.Fatalf("run status=%d body=%s", response.StatusCode, body)
	}
	if providerCalls.Load() != 2 || !secondRequestHadTaskOutput.Load() {
		t.Fatalf("provider calls=%d, second request had task output=%v", providerCalls.Load(), secondRequestHadTaskOutput.Load())
	}
	stored, err := os.ReadFile(filepath.Join(taskStorage, "tasks.json"))
	if err != nil || !strings.Contains(string(stored), `"id":"T-001"`) || !strings.Contains(string(stored), `"status":"in-progress"`) {
		t.Fatalf("persisted task storage=%s, error=%v", stored, err)
	}
	transcriptFile, err := os.Open(filepath.Join(sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcriptFile.Close()
	var lines []string
	scanner := bufio.NewScanner(transcriptFile)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 4 || !strings.Contains(strings.Join(lines, "\n"), `"call_id":"call-task-add"`) || !strings.Contains(strings.Join(lines, "\n"), "task_id: T-001") {
		t.Fatalf("messages.jsonl does not include task call and output: %q", lines)
	}
}
