package app

import (
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

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// hostedToolsProvider is a fake Responses endpoint that answers like a provider with a
// built-in search: a web_search_call item, then a message with a url_citation.
type hostedToolsProvider struct {
	mu       sync.Mutex
	requests []hostedToolsRequest
}

type hostedToolsRequest struct {
	Tools []json.RawMessage `json:"tools"`
	Input []json.RawMessage `json:"input"`
	// HasToolsKey is true when the request body carried a "tools" key at all.
	HasToolsKey bool `json:"-"`
}

func (p *hostedToolsProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request hostedToolsRequest
		raw, err := io.ReadAll(r.Body)
		if err != nil || json.Unmarshal(raw, &request) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		_, request.HasToolsKey = keys["tools"]
		p.mu.Lock()
		p.requests = append(p.requests, request)
		n := len(p.requests)
		p.mu.Unlock()
		search := fmt.Sprintf(`{"id":"ws_%d","type":"web_search_call","status":"completed","action":{"type":"search","query":"go release"}}`, n)
		message := fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d","annotations":[{"type":"url_citation","url":"https://go.dev/dl","title":"Downloads","start_index":0,"end_index":8}]}]}`, n)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":` + search + `}`,
			`{"type":"response.output_item.done","output_index":1,"item":` + message + `}`,
			`{"type":"response.completed","response":{"id":"resp-hosted","status":"completed","model":"gpt-test","output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}
}

func (p *hostedToolsProvider) snapshot() []hostedToolsRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]hostedToolsRequest(nil), p.requests...)
}

func toolTypes(tools []json.RawMessage) []string {
	types := []string{}
	for _, tool := range tools {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(tool, &head)
		types = append(types, head.Type)
	}
	return types
}

func TestHostedToolsReachTheProviderAndTheirItemsRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &hostedToolsProvider{}
	server := httptest.NewServer(provider.handler(t))
	defer server.Close()

	writeModel := func(hosted string) {
		model := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q%s}`, server.URL+"/v1", hosted)
		if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(model), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeModel(`,"hosted_tools":[{"type":"web_search","search_context_size":"low"}]`)
	if err := os.WriteFile(filepath.Join(workspace, "setting.json"), []byte(`{"session_roots":[{"id":"local","path":"sessions"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(fmt.Sprintf(`{"cwd":%q}`, workspace)), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: workspace, Environment: map[string]string{
		"AGENT_API_TOKEN": integrationAPIToken, "FIXTURE_OPENAI_KEY": "k",
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(service.Handler)
	defer func() { service.Close(); api.Close() }()

	run := func(input string) string {
		request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"`+input+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: completed") {
			t.Fatalf("run %q: status %d body %s", input, response.StatusCode, body)
		}
		return string(body)
	}

	run("first")
	run("second")
	requests := provider.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	// 1. The session exposes no function tool, and the hosted tool still arrives, unchanged.
	if got := toolTypes(requests[0].Tools); len(got) != 1 || got[0] != "web_search" {
		t.Fatalf("first request tools = %v, want [web_search]", got)
	}
	if !strings.Contains(string(requests[0].Tools[0]), `"search_context_size":"low"`) {
		t.Fatalf("hosted tool fields were not passed on: %s", requests[0].Tools[0])
	}
	// 2. The provider's own items were stored and are replayed as input on the next turn.
	var replayedSearch, replayedCitation bool
	for _, item := range requests[1].Input {
		text := string(item)
		replayedSearch = replayedSearch || strings.Contains(text, `"type":"web_search_call"`) && strings.Contains(text, `"id":"ws_1"`)
		replayedCitation = replayedCitation || strings.Contains(text, `"url_citation"`) && strings.Contains(text, "answer 1")
	}
	if !replayedSearch || !replayedCitation {
		t.Fatalf("second request input lacks the stored provider items: web_search_call=%t url_citation=%t\n%v", replayedSearch, replayedCitation, requests[1].Input)
	}
	transcript, err := os.ReadFile(filepath.Join(sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "web_search_call") || !strings.Contains(string(transcript), "url_citation") {
		t.Fatalf("messages.jsonl lacks the provider items:\n%s", transcript)
	}

	// 3. Editing model.json takes effect on the next run: the cached runner is replaced.
	writeModel(`,"hosted_tools":[{"type":"web_search"},{"type":"file_search"}]`)
	run("third")
	requests = provider.snapshot()
	if got := toolTypes(requests[2].Tools); strings.Join(got, ",") != "web_search,file_search" {
		t.Fatalf("after editing model.json the tools = %v, want [web_search file_search]", got)
	}
	// 4. Removing the key removes the tools: the request has no tools key content.
	writeModel(``)
	run("fourth")
	requests = provider.snapshot()
	if requests[3].HasToolsKey {
		t.Fatalf("after removing hosted_tools the request still has a tools key: %v", requests[3].Tools)
	}
}
