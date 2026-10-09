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
	"testing"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// A provider-hosted search must reach the Web UI both live (SSE) and from the
// stored transcript (session snapshot), with its sources as citations on the answer.
func TestHostedSearchReachesTheWebUIFromStreamAndSnapshot(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &hostedToolsProvider{}
	server := httptest.NewServer(provider.handler(t))
	defer server.Close()

	model := fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q,"hosted_tools":[{"type":"web_search"}]}`, server.URL+"/v1")
	files := map[string]string{
		filepath.Join(sessionDir, "model.json"):   model,
		filepath.Join(workspace, "setting.json"):  `{"session_roots":[{"id":"local","path":"sessions"}]}`,
		filepath.Join(sessionDir, "setting.json"): fmt.Sprintf(`{"cwd":%q}`, workspace),
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
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

	do := func(method, path, body string) (int, string) {
		request, err := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw)
	}

	status, stream := do(http.MethodPost, "/api/sessions/local:demo/run", `{"input":"what is new in go"}`)
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("run: status %d body %s", status, stream)
	}

	// Live: the completed message carries the search part and the cited text.
	completed := lastSSEData(t, stream, "completed")
	var event struct {
		Message struct {
			Parts []map[string]any `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(completed), &event); err != nil {
		t.Fatalf("completed event %s: %v", completed, err)
	}
	assertSearchAndCitations(t, "completed event", event.Message.Parts)

	// Stored: the session snapshot rebuilds the same parts from messages.jsonl.
	// (The /history route serves only the compaction backup.)
	status, snapshot := do(http.MethodGet, "/api/sessions/local:demo", "")
	if status != http.StatusOK {
		t.Fatalf("snapshot: status %d body %s", status, snapshot)
	}
	var page struct {
		Messages []struct {
			Sender string           `json:"sender"`
			Parts  []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(snapshot), &page); err != nil {
		t.Fatalf("snapshot %s: %v", snapshot, err)
	}
	var assistant []map[string]any
	for _, message := range page.Messages {
		if message.Sender == "assistant" {
			assistant = message.Parts
		}
	}
	assertSearchAndCitations(t, "session snapshot", assistant)
}

func lastSSEData(t *testing.T, stream, name string) string {
	t.Helper()
	data := ""
	lines := strings.Split(stream, "\n")
	for index, line := range lines {
		if line == "event: "+name && index+1 < len(lines) {
			data = strings.TrimPrefix(lines[index+1], "data: ")
		}
	}
	if data == "" {
		t.Fatalf("no %q event in stream:\n%s", name, stream)
	}
	return data
}

func assertSearchAndCitations(t *testing.T, where string, parts []map[string]any) {
	t.Helper()
	var search, text map[string]any
	for _, part := range parts {
		switch part["type"] {
		case "web_search":
			search = part
		case "text":
			text = part
		}
	}
	callID, _ := search["callId"].(string)
	if search == nil || search["query"] != "go release" || search["status"] != "completed" || callID == "" {
		t.Fatalf("%s: web_search part = %v (parts %v)", where, search, parts)
	}
	citations, _ := text["citations"].([]any)
	if text == nil || len(citations) != 1 {
		t.Fatalf("%s: text part = %v (parts %v)", where, text, parts)
	}
	first, _ := citations[0].(map[string]any)
	if first["url"] != "https://go.dev/dl" || first["title"] != "Downloads" {
		t.Fatalf("%s: citation = %v", where, first)
	}
}
