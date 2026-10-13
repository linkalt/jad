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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
	"jad/backend-go/internal/tools/native"
)

func TestHTTPRunExecutesNativeProcessStartAndWaitAcrossProviderRounds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	processStorage := filepath.Join(workspace, "processes")
	processTool, err := native.NewProcessTool(sessionfs.NativeProcessSettings{
		Policy:           sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}},
		Shell:            "/bin/sh",
		MaxRunning:       2,
		MaxCompleted:     10,
		WaitTimeoutMs:    1000,
		MaxWaitTimeoutMs: 2500,
		Stdout:           sessionfs.NativeBashStreamBudget{HeadBytes: 128, TailBytes: 128},
		Stderr:           sessionfs.NativeBashStreamBudget{HeadBytes: 128, TailBytes: 128},
		LogCapBytes:      4096,
		MaxLineChars:     1000,
		StorageDir:       processStorage,
		Env:              map[string]string{},
		EnvDeny:          []string{},
	}, nil, "FIXTURE_OPENAI_KEY")
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
	var secondRequestHadStartResult atomic.Bool
	var thirdRequestHadWaitOutput atomic.Bool
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
			if len(request.Tools) != 1 || !strings.Contains(string(request.Tools[0]), `"name":"process"`) {
				http.Error(w, "native process definition missing", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-process-start","name":"process","arguments":"{\"action\":\"start\",\"command\":\"printf 'process-output'; sleep 0.15\"}"}`
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"resp-process-start","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 2:
			startOutput, found := findFunctionOutput(request.Input, "call-process-start")
			if !found {
				http.Error(w, "start tool output missing from second request", http.StatusBadRequest)
				return
			}
			processID := processIDFromToolOutput(startOutput)
			if processID == "" {
				http.Error(w, "process id missing from start tool output", http.StatusBadRequest)
				return
			}
			secondRequestHadStartResult.Store(true)
			arguments, err := json.Marshal(struct {
				Action    string `json:"action"`
				ID        string `json:"id"`
				TimeoutMS int    `json:"timeout_ms"`
			}{Action: "wait", ID: processID, TimeoutMS: 2000})
			if err != nil {
				http.Error(w, "could not encode wait arguments", http.StatusInternalServerError)
				return
			}
			item, err := json.Marshal(struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Type: "function_call", CallID: "call-process-wait", Name: "process", Arguments: string(arguments)})
			if err != nil {
				http.Error(w, "could not encode wait call", http.StatusInternalServerError)
				return
			}
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + string(item) + `}`,
				`{"type":"response.completed","response":{"id":"resp-process-wait","status":"completed","model":"gpt-test","output":[]}}`,
			}
		case 3:
			waitOutput, found := findFunctionOutput(request.Input, "call-process-wait")
			if !found || !strings.Contains(waitOutput, "process-output") {
				http.Error(w, "wait output missing from third request", http.StatusBadRequest)
				return
			}
			thirdRequestHadWaitOutput.Store(true)
			events = []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}}`,
				`{"type":"response.completed","response":{"id":"resp-process-final","status":"completed","model":"gpt-test","output":[]}}`,
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

	config, fixture := layoutConfig(t, layouttest.Spec{
		CWD:                 workspace,
		ToolDefinitionPaths: []string{filepath.Join(workspace, "process.json")},
		Settings:            map[string]any{"max_tool_rounds": 3},
		Native:              map[string]any{"process": map[string]any{"shell": "/bin/sh", "storage_dir": processStorage, "max_running": 2, "wait_timeout_ms": 1000, "max_wait_timeout_ms": 2500, "stdout": map[string]any{"head_bytes": 128, "tail_bytes": 128}, "stderr": map[string]any{"head_bytes": 128, "tail_bytes": 128}, "log_cap_bytes": 4096, "max_line_chars": 1000, "strip_ansi": false}},
		Models:              map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1"}},
	}, map[string]string{"JAD_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "local-provider-key"})
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() {
		service.Close()
		api.Close()
	}()

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"start a process"}`))
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
	if !secondRequestHadStartResult.Load() || !thirdRequestHadWaitOutput.Load() {
		t.Fatalf("provider inputs missing process tool output: start=%t wait=%t", secondRequestHadStartResult.Load(), thirdRequestHadWaitOutput.Load())
	}
	transcript, err := os.Open(filepath.Join(profileState(fixture), "messages.jsonl"))
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
	if len(lines) != 6 || !strings.Contains(lines[2], `"call_id":"call-process-start"`) || !strings.Contains(lines[4], `"call_id":"call-process-wait"`) || !strings.Contains(lines[4], "process-output") {
		t.Fatalf("messages.jsonl records = %q, want persisted start/wait outputs", lines)
	}
}

func findFunctionOutput(input []json.RawMessage, callID string) (string, bool) {
	for _, item := range input {
		var output struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		if json.Unmarshal(item, &output) == nil && output.Type == "function_call_output" && output.CallID == callID {
			return output.Output, true
		}
	}
	return "", false
}

func processIDFromToolOutput(output string) string {
	const marker = "process_id: "
	_, remaining, found := strings.Cut(output, marker)
	if !found {
		return ""
	}
	id, _, _ := strings.Cut(remaining, "\n")
	return strings.TrimSpace(id)
}
