package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

func TestHTTPRunExecutesConfiguredNativeBashAndContinuesProviderRound(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bashTool, err := native.NewBashTool(sessionfs.NativeBashSettings{Shell: shell}, "FIXTURE_OPENAI_KEY")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int   `json:"schema_version"`
		Tools         []any `json:"tools"`
	}{SchemaVersion: 1, Tools: []any{bashTool.Definition}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "bash.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var providerCalls atomic.Int32
	var secondRequestHadBashResult atomic.Bool
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
			if len(request.Tools) != 1 || !strings.Contains(string(request.Tools[0]), `"name":"bash"`) {
				http.Error(w, "native bash definition missing", http.StatusBadRequest)
				return
			}
			item := `{"type":"function_call","call_id":"call-bash","name":"bash","arguments":"{\"command\":\"printf 'bash fixture output\\\\n'\"}"}`
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
				if json.Unmarshal(item, &result) == nil && result.Type == "function_call_output" && result.CallID == "call-bash" && strings.Contains(result.Output, "bash fixture output") {
					secondRequestHadBashResult.Store(true)
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

	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionSetting, err := json.Marshal(map[string]any{
		"cwd": workspace, "max_tool_rounds": 2, "tool_definition_paths": []string{"bash.json"},
		"tools": map[string]any{"native": map[string]any{"bash": map[string]any{"shell": shell, "spill": map[string]any{"enabled": false}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), sessionSetting, 0o600); err != nil {
		t.Fatal(err)
	}
	modelSetting, err := json.Marshal(map[string]any{
		"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_OPENAI_KEY", "base_url": provider.URL + "/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), modelSetting, 0o600); err != nil {
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

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"run a short shell command"}`))
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
	if !secondRequestHadBashResult.Load() {
		t.Fatal("second provider request did not include the native bash result")
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
