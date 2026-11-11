package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// translatorFixture is the layout fixture with dev/coder's model served by a fake
// provider of another protocol, and the skill tool on (its hello skill says "file body 42").
func translatorFixture(t *testing.T, model map[string]any) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	for rel, value := range map[string]any{"models/openai/small.json": model, "tools/coding/none.json": map[string]any{"native": map[string]any{"skill": map[string]any{}}, "mcp": map[string]any{}}} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(f.root, rel), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(t, f.root, "hello", "file body 42")
	return f
}

// recorded keeps the requests a fake provider received.
type recorded struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
}

func (r *recorded) add(request *http.Request) string {
	data, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, string(data))
	r.headers = append(r.headers, request.Header.Clone())
	return string(data)
}

func runCoder(t *testing.T, f *layoutFixture) headless.Result {
	t.Helper()
	result, err := headlessFor(t, f).Run(context.Background(), headless.Request{Session: "local:demo", Profile: "dev/coder", Input: "read hello.txt"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result
}

func TestChatCompletionsModelRunsAToolRoundInProcess(t *testing.T) {
	requests := &recorded{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := requests.add(r)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"id":"chatcmpl-1","model":"local-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"skill","arguments":"{\"name\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`,
		}
		if strings.Contains(body, `"role":"tool"`) {
			events = []string{
				`{"id":"chatcmpl-2","model":"local-model","choices":[{"index":0,"delta":{"reasoning_content":"The file says 42."}}]}`,
				`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":"It says 42."},"finish_reason":"stop"}]}`,
				`{"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":6,"total_tokens":46}}`,
			}
		}
		for _, event := range append(events, "[DONE]") {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(provider.Close)
	f := translatorFixture(t, map[string]any{"api": "chat_completions", "model": "local-model", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"})

	result := runCoder(t, f)
	if result.Status != headless.StatusCompleted || result.Text != "It says 42." {
		t.Fatalf("result = %+v", result)
	}
	if len(requests.bodies) != 2 || requests.headers[0].Get("Authorization") != "Bearer k" {
		t.Fatalf("requests = %d, auth %q", len(requests.bodies), requests.headers[0].Get("Authorization"))
	}
	var second struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(requests.bodies[1]), &second); err != nil {
		t.Fatal(err)
	}
	last := second.Messages[len(second.Messages)-1]
	if second.Model != "local-model" || len(second.Tools) == 0 || last["role"] != "tool" || last["tool_call_id"] != "call_r" || !strings.Contains(fmt.Sprint(last["content"]), "file body 42") {
		t.Fatalf("second request: model %q, %d tools, last message %v", second.Model, len(second.Tools), last)
	}
	if !strings.Contains(string(result.Usage), `"input_tokens":`) {
		t.Fatalf("usage = %s", result.Usage)
	}
	transcript, err := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl"))
	if err != nil || !strings.Contains(string(transcript), `"call_id":"call_r"`) || !strings.Contains(string(transcript), "It says 42.") {
		t.Fatalf("transcript: %v\n%s", err, transcript)
	}
}

func TestAnthropicModelRunsAToolRoundInProcess(t *testing.T) {
	requests := &recorded{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := requests.add(r)
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":30,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_r","name":"skill","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"name\":\"hello\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`,
			`{"type":"message_stop"}`,
		}
		if strings.Contains(body, `"tool_result"`) {
			events = []string{
				`{"type":"message_start","message":{"id":"msg_2","model":"claude-test","usage":{"input_tokens":50,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"It says 42."}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			}
		}
		for _, event := range events {
			var kind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(event), &kind)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind.Type, event)
		}
	}))
	t.Cleanup(provider.Close)
	f := translatorFixture(t, map[string]any{
		"api": "anthropic_messages", "model": "claude-test", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL,
		"anthropic": map[string]any{"default_max_tokens": 2048, "upstream": map[string]any{"auth_header": "x-api-key"}},
	})

	result := runCoder(t, f)
	if result.Status != headless.StatusCompleted || result.Text != "It says 42." {
		t.Fatalf("result = %+v", result)
	}
	if len(requests.bodies) != 2 || requests.headers[0].Get("X-Api-Key") != "k" || requests.headers[0].Get("Anthropic-Version") == "" {
		t.Fatalf("requests = %d, headers %v", len(requests.bodies), requests.headers)
	}
	var second struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(requests.bodies[1]), &second); err != nil {
		t.Fatal(err)
	}
	last := second.Messages[len(second.Messages)-1]
	if second.Model != "claude-test" || second.MaxTokens != 2048 || last.Role != "user" || last.Content[0]["type"] != "tool_result" || last.Content[0]["tool_use_id"] != "toolu_r" || !strings.Contains(fmt.Sprint(last.Content[0]["content"]), "file body 42") {
		t.Fatalf("second request = %s", requests.bodies[1])
	}
	// The model file sets no cache, so the in-process default marks the last tool and the
	// last conversation block (the tool result); nothing else.
	var marks struct {
		Tools []struct {
			Name         string         `json:"name"`
			CacheControl map[string]any `json:"cache_control"`
		} `json:"tools"`
	}
	_ = json.Unmarshal([]byte(requests.bodies[1]), &marks)
	if len(marks.Tools) == 0 || marks.Tools[len(marks.Tools)-1].CacheControl["type"] != "ephemeral" || last.Content[0]["cache_control"] == nil {
		t.Fatalf("cache breakpoints missing: %s", requests.bodies[1])
	}
	if count := strings.Count(requests.bodies[1], `"cache_control"`); count > 4 {
		t.Fatalf("%d breakpoints, Anthropic allows 4: %s", count, requests.bodies[1])
	}
}

// An HTTP error from the provider reaches the run with its status, through the
// translator: the Responses client sees the status the provider answered.
func TestTranslatedProviderErrorFailsTheRun(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"context length exceeded","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	}))
	t.Cleanup(provider.Close)
	f := translatorFixture(t, map[string]any{"api": "chat_completions", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"})
	result := runCoder(t, f)
	// The run error names the status; jad withholds provider response bodies from it.
	if result.Status != headless.StatusFailed || !strings.Contains(result.Error, "HTTP 400") {
		t.Fatalf("result = %+v", result)
	}
}

// An Anthropic model always needs its API key in the environment: the adapter holds no
// credential of its own (the credential-file sign-in was removed), so a leftover
// credential_file in its settings must not stand in for the key.
func TestAnthropicModelNeedsItsEnvironmentKey(t *testing.T) {
	config := sessionfs.SessionConfig{Model: sessionfs.Model{API: sessionfs.APIAnthropicMessages, APIKeyEnv: "ANTHROPIC_API_KEY"}}
	for _, settings := range []string{``, `{"upstream":{"credential_file":"/tmp/none.json"}}`} {
		config.Model.Anthropic = json.RawMessage(settings)
		resolver := &Resolver{environment: map[string]string{}}
		if _, err := resolver.providerAPIKey(config); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Fatalf("settings %q without the key: %v", settings, err)
		}
		resolver = &Resolver{environment: map[string]string{"ANTHROPIC_API_KEY": " sk-ant-api03-x "}}
		if key, err := resolver.providerAPIKey(config); err != nil || key != "sk-ant-api03-x" {
			t.Fatalf("settings %q with the key: %q %v", settings, key, err)
		}
	}
}
