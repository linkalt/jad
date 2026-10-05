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

func TestHTTPRunExecutesConfiguredNativeReadAndContinuesProviderRound(t *testing.T) {
	workspace := t.TempDir()
	sessionsDir := filepath.Join(workspace, "sessions")
	sessionDir := filepath.Join(sessionsDir, "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("sample content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readSettings := sessionfs.NativeReadSettings{
		Policy:       sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		MaxBytes:     1024,
		MaxFileBytes: 4096,
		DefaultLimit: 250,
	}
	readTool, err := native.NewReadTool(readSettings)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{readTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "read.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var secondRequestHadReadResult atomic.Bool
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
		call := providerCalls.Add(1)
		var events []string
		if call == 1 {
			if len(request.Tools) != 1 || !strings.Contains(string(request.Tools[0]), `"name":"read"`) {
				http.Error(w, "native read definition missing", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-read","name":"read","arguments":"{\"path\":\"sample.txt\"}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-tool","status":"completed","model":"gpt-test","output":[]}}`,
			}
		} else if call == 2 {
			for _, item := range request.Input {
				var result struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}
				if json.Unmarshal(item, &result) == nil && result.Type == "function_call_output" && result.CallID == "call-read" && strings.Contains(result.Output, "sample content") {
					secondRequestHadReadResult.Store(true)
				}
			}
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}}`,
				`{"type":"response.completed","response":{"id":"resp-final","status":"completed","model":"gpt-test","output":[]}}`,
			}
		} else {
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
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":2,"tool_definition_paths":["read.json"],"tools":{"native":{"read":{"max_bytes":1024,"max_file_bytes":4096,"default_limit":250}}}}`, workspace)
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

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"read sample.txt"}`))
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, body=%s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "event: completed") || !strings.Contains(string(body), "finished") {
		t.Fatalf("run stream did not complete with final provider response: %s", body)
	}
	if calls := providerCalls.Load(); calls != 2 {
		t.Fatalf("provider requests = %d, want 2", calls)
	}
	if !secondRequestHadReadResult.Load() {
		t.Fatal("second provider request did not include the persisted native read result")
	}

	transcript, err := os.Open(filepath.Join(sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	var lines []string
	scanner := bufio.NewScanner(transcript)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 || !strings.Contains(lines[2], `"type":"function_call_output"`) {
		t.Fatalf("messages.jsonl records = %q, want input, tool-call response, tool output, final response", lines)
	}
}

func TestHTTPRunExecutesConfiguredNativeWriteAndEditAcrossRounds(t *testing.T) {
	workspace := t.TempDir()
	sessionsDir := filepath.Join(workspace, "sessions")
	sessionDir := filepath.Join(sessionsDir, "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(sessionDir, "backups")
	mutationSettings := sessionfs.NativeMutationSettings{
		BackupDir:           &backupDir,
		BackupMaxCount:      5,
		BackupMaxTotalBytes: 10 * 1024 * 1024,
		Fsync:               true,
	}
	writeTool, err := native.NewWriteTool(sessionfs.NativeWriteSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, MaxBytes: 1024, NativeMutationSettings: mutationSettings,
	}, native.NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := native.NewEditTool(sessionfs.NativeEditSettings{
		Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, MaxBytes: 1024, NativeMutationSettings: mutationSettings,
	}, native.NewPathLocks())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{writeTool.Definition, editTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tools.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var secondRequestHadWriteResult atomic.Bool
	var thirdRequestHadEditResult atomic.Bool
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
		call := providerCalls.Add(1)
		var events []string
		switch call {
		case 1:
			if len(request.Tools) != 2 || !strings.Contains(string(request.Tools[0]), `"name":"write"`) || !strings.Contains(string(request.Tools[1]), `"name":"edit"`) {
				http.Error(w, "native write/edit definitions missing or out of order", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-write","name":"write","arguments":"{\"path\":\"source.txt\",\"content\":\"before\\n\"}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-write","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 2:
			for _, item := range request.Input {
				var result struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}
				if json.Unmarshal(item, &result) == nil && result.Type == "function_call_output" && result.CallID == "call-write" && strings.Contains(result.Output, "Created source.txt") {
					secondRequestHadWriteResult.Store(true)
				}
			}
			item := `{"type":"function_call","call_id":"call-edit","name":"edit","arguments":"{\"path\":\"source.txt\",\"oldText\":\"before\\n\",\"newText\":\"after\\n\"}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-edit","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 3:
			for _, item := range request.Input {
				var result struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}
				if json.Unmarshal(item, &result) == nil && result.Type == "function_call_output" && result.CallID == "call-edit" && strings.Contains(result.Output, "Edited source.txt") {
					thirdRequestHadEditResult.Store(true)
				}
			}
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}}`,
				`{"type":"response.completed","response":{"id":"resp-final","status":"completed","model":"gpt-test","output":[]}}`,
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
	sessionSetting := fmt.Sprintf(`{"cwd":%q,"max_tool_rounds":3,"tool_definition_paths":["tools.json"],"tools":{"native":{"write":{"max_bytes":1024},"edit":{"max_bytes":1024}}}}`, workspace)
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

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"write and edit source.txt"}`))
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, body=%s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "event: completed") || !strings.Contains(string(body), "finished") {
		t.Fatalf("run stream did not complete with final provider response: %s", body)
	}
	if calls := providerCalls.Load(); calls != 3 {
		t.Fatalf("provider requests = %d, want 3", calls)
	}
	if !secondRequestHadWriteResult.Load() || !thirdRequestHadEditResult.Load() {
		t.Fatalf("provider round inputs missing tool results: write=%t edit=%t", secondRequestHadWriteResult.Load(), thirdRequestHadEditResult.Load())
	}
	file, err := os.ReadFile(filepath.Join(workspace, "source.txt"))
	if err != nil || string(file) != "after\n" {
		t.Fatalf("final file = %q, error = %v", file, err)
	}
	backups, err := filepath.Glob(filepath.Join(sessionDir, "backups", "source.txt.bak-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("edit backups = %v, error = %v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || string(backup) != "before\n" {
		t.Fatalf("edit backup = %q, error = %v", backup, err)
	}

	transcript, err := os.Open(filepath.Join(sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer transcript.Close()
	var lines []string
	scanner := bufio.NewScanner(transcript)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 6 || !strings.Contains(lines[2], `"call_id":"call-write"`) || !strings.Contains(lines[4], `"call_id":"call-edit"`) {
		t.Fatalf("messages.jsonl records = %q, want input, write call/output, edit call/output, final response", lines)
	}
}
