package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionfs/layouttest"
	"example.com/agent-harness/backend-go/internal/tools/native"
	"example.com/agent-harness/backend-go/internal/transcript"
)

// sessionStartFixture runs a session whose only configured trigger is session_start and
// returns, for each provider request, whether it carried a developer state block.
type sessionStartFixture struct {
	t      *testing.T
	api    *httptest.Server
	config Config
	// processStorage is the directory of the process manifests the provider lists.
	processStorage string
	taskStore      *native.TaskStore
	taskSettings   sessionfs.NativeTaskSettings
	mu             sync.Mutex
	// failProvider makes the provider answer every request with a 400.
	failProvider atomic.Bool
	// onProviderRequest, when set before a run, is called as each provider request arrives.
	onProviderRequest func()
	requests          []bool
	// sawStale is set when any request carried the text of the stale test block.
	sawStale bool
}

func newSessionStartFixture(t *testing.T, seedProcess bool) *sessionStartFixture {
	t.Helper()
	return newStateContextFixture(t, seedProcess, map[string]any{"session_start": map[string]any{"tools": []string{"process"}, "max_bytes": 4096}})
}

func newStateContextFixture(t *testing.T, seedProcess bool, triggers map[string]any) *sessionStartFixture {
	t.Helper()
	return newStateContextFixtureWith(t, seedProcess, triggers, nil, nil)
}

// newStateContextFixtureWith adds session settings and a server logger to the fixture.
func newStateContextFixtureWith(t *testing.T, seedProcess bool, triggers, extraSettings map[string]any, logger *slog.Logger) *sessionStartFixture {
	t.Helper()
	fixture := &sessionStartFixture{t: t}
	workspace := t.TempDir()
	processStorage := filepath.Join(workspace, "processes")
	fixture.processStorage = processStorage
	processTool, err := native.NewProcessTool(sessionfs.NativeProcessSettings{StorageDir: processStorage, Shell: "/bin/sh"}, nil, "UNSET_PROVIDER_KEY")
	if err != nil {
		t.Fatal(err)
	}
	taskStorage := filepath.Join(workspace, "tasks")
	fixture.taskSettings = sessionfs.NativeTaskSettings{
		MaxTasks: 10, MaxStorageBytes: 64 * 1024,
		StateContext: sessionfs.NativeTaskStateContextSettings{Statuses: []string{"in-progress"}, Limit: 5},
	}
	fixture.taskStore = native.NewTaskStore(taskStorage)
	taskTool, err := native.NewTaskTool(fixture.taskSettings, fixture.taskStore)
	if err != nil {
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
	if seedProcess {
		writeProcessManifest(t, processStorage, native.ProcessManifest{
			SchemaVersion: 1, ID: "proc_saved", State: native.ProcessExited, CWD: "/workspace/project",
			StartedAt: "2026-10-02T10:00:00.000Z", EndedAt: "2026-10-02T10:00:01.000Z",
			ExitCode: json.RawMessage("0"), Signal: json.RawMessage("null"),
		})
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixture.onProviderRequest != nil {
			fixture.onProviderRequest()
		}
		if fixture.failProvider.Load() {
			http.Error(w, `{"error":{"message":"provider said no"}}`, http.StatusBadRequest)
			return
		}
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		carried := false
		stale := false
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
				carried = carried || (strings.Contains(content.Text, "STATE CONTEXT SNAPSHOT") && strings.Contains(content.Text, "proc_saved"))
				stale = stale || strings.Contains(content.Text, "no longer configured")
			}
		}
		fixture.mu.Lock()
		fixture.sawStale = fixture.sawStale || stale
		fixture.requests = append(fixture.requests, carried)
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-start","status":"completed","model":"gpt-test","usage":{"input_tokens":5},"output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	t.Cleanup(provider.Close)

	settings := map[string]any{
		"max_tool_rounds": 1,
		"statecontext":    map[string]any{"enabled": true, "triggers": triggers},
	}
	for key, value := range extraSettings {
		settings[key] = value
	}
	config, _ := layoutConfig(t, layouttest.Spec{
		SessionID:           "one",
		CWD:                 workspace,
		ToolDefinitionPaths: []string{filepath.Join(workspace, "process.json")},
		Settings:            settings,
		Native: map[string]any{
			"process": map[string]any{"shell": "/bin/sh", "storage_dir": processStorage, "statecontext": map[string]any{"states": []string{"exited"}, "limit": 5}},
			"task":    map[string]any{"storage_dir": taskStorage, "max_tasks": 10, "statecontext": map[string]any{"statuses": []string{"in-progress"}, "limit": 5}},
		},
		Models: map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	if logger != nil {
		config.HTTP.Logger = logger
	}
	fixture.config = config
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixture.api = httptest.NewServer(service.Handler)
	t.Cleanup(func() {
		service.Close()
		fixture.api.Close()
	})
	return fixture
}

func (fixture *sessionStartFixture) run(input string) {
	fixture.t.Helper()
	request, err := http.NewRequest(http.MethodPost, fixture.api.URL+"/api/sessions/local:one/run", strings.NewReader(`{"input":"`+input+`"}`))
	if err != nil {
		fixture.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fixture.t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		fixture.t.Fatalf("run %q: status=%d err=%v body=%s", input, response.StatusCode, err, body)
	}
}

func (fixture *sessionStartFixture) sessionConfig() sessionfs.SessionConfig {
	fixture.t.Helper()
	location, err := sessionfs.Resolve(fixture.config.Roots, "local:one")
	if err != nil {
		fixture.t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		fixture.t.Fatal(err)
	}
	return sessionConfig
}

func (fixture *sessionStartFixture) snapshotPath() string {
	fixture.t.Helper()
	return fixture.sessionConfig().StoragePaths.StateContext
}

func (fixture *sessionStartFixture) messagesPath() string {
	fixture.t.Helper()
	return fixture.sessionConfig().StoragePaths.Messages
}

func TestHTTPRunWritesSessionStartStateContextBeforeAnyCompaction(t *testing.T) {
	fixture := newSessionStartFixture(t, true)
	fixture.run("first")
	fixture.run("second")

	if len(fixture.requests) != 2 || !fixture.requests[0] || !fixture.requests[1] {
		t.Fatalf("requests carrying the state block = %v; want both runs to carry it", fixture.requests)
	}
	data, err := os.ReadFile(fixture.snapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot transcript.StateContextSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Trigger != "session_start" || !strings.Contains(snapshot.Text, "proc_saved") {
		t.Fatalf("persisted snapshot = %#v; want the session_start trigger with the process", snapshot)
	}
}

func TestHTTPRunWritesNoSessionStartStateContextWhenProvidersAreEmpty(t *testing.T) {
	fixture := newSessionStartFixture(t, false)
	fixture.run("first")

	if len(fixture.requests) != 1 || fixture.requests[0] {
		t.Fatalf("requests carrying the state block = %v; want none", fixture.requests)
	}
	if _, err := os.Stat(fixture.snapshotPath()); !os.IsNotExist(err) {
		t.Fatalf("snapshot file stat error = %v; want it not to exist", err)
	}
}

func TestHTTPRunIgnoresStoredStateContextWhoseTriggerIsNotConfigured(t *testing.T) {
	fixture := newSessionStartFixture(t, true)
	stale := transcript.StateContextEntry{
		StateContextSnapshot: transcript.StateContextSnapshot{
			SchemaVersion: 1, GeneratedAt: "2026-10-01T00:00:00.000Z", Trigger: "compaction",
			Text: "STATE CONTEXT SNAPSHOT\nstored by a trigger that is no longer configured",
		},
		Role: "developer",
	}
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	path := fixture.snapshotPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.run("first")

	if len(fixture.requests) != 1 || !fixture.requests[0] || fixture.sawStale {
		t.Fatalf("carried session_start block = %v, stale block sent = %v; want the new block and not the stale one", fixture.requests, fixture.sawStale)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("state context file has %d lines, want the stale one and the new session_start one:\n%s", len(lines), data)
	}
	var added transcript.StateContextEntry
	if err := json.Unmarshal([]byte(lines[1]), &added); err != nil {
		t.Fatal(err)
	}
	if added.Trigger != "session_start" || !strings.Contains(added.Text, "proc_saved") || added.After != 1 {
		t.Fatalf("new entry = %#v; want session_start with the process, placed after the first input", added)
	}
}
