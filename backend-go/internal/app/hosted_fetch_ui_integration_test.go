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

// A provider-hosted fetch is not special-cased anywhere: its item reaches the Web
// UI as a generic hosted_tool part, live and from the stored transcript, with its
// large page text shortened to server.max_inline_part_bytes and the full size
// reported. The same run also shows a search, which uses the same code path.
func TestHostedFetchReachesTheWebUIAsAGenericCappedPart(t *testing.T) {
	const inlineCap = 2048
	const pageBytes = 60000
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, "sessions", "demo")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := strings.Repeat("page text ", pageBytes/10)
	fetch := fmt.Sprintf(`{"id":"st_1","type":"openrouter:web_fetch","status":"completed","url":"https://go.dev/dl/","title":"All releases","httpStatus":200,"content":%q}`, page)
	search := `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go release"}}`
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.26.5"}]}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		announced := `{"id":"st_1","type":"openrouter:web_fetch","status":"in_progress"}`
		for _, event := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":` + announced + `}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + fetch + `}`,
			`{"type":"response.output_item.done","output_index":1,"item":` + search + `}`,
			`{"type":"response.output_item.done","output_index":2,"item":` + message + `}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"gpt-test","output":[` + fetch + `,` + search + `,` + message + `]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	files := map[string]string{
		filepath.Join(sessionDir, "model.json"):   fmt.Sprintf(`{"provider":"openai","model":"gpt-test","api_key_env":"FIXTURE_OPENAI_KEY","base_url":%q,"hosted_tools":[{"type":"web_fetch"}]}`, provider.URL+"/v1"),
		filepath.Join(workspace, "setting.json"):  fmt.Sprintf(`{"session_roots":[{"id":"local","path":"sessions"}],"server":{"max_inline_part_bytes":%d}}`, inlineCap),
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

	status, stream := do(http.MethodPost, "/api/sessions/local:demo/run", `{"input":"fetch the go downloads page"}`)
	if status != http.StatusOK || !strings.Contains(stream, "event: completed") {
		t.Fatalf("run: status %d body %.400s", status, stream)
	}

	type part = map[string]any
	var live struct {
		Message struct {
			Parts []part `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lastSSEData(t, stream, "completed")), &live); err != nil {
		t.Fatal(err)
	}
	assertFetchPart(t, "completed event", live.Message.Parts, inlineCap, pageBytes)

	// Nothing streamed to the browser exceeds the cap by much: the 60 KB page is
	// never sent whole, in any event.
	if strings.Count(stream, "page text page text") > 0 && len(stream) > 20*inlineCap {
		t.Fatalf("the stream is %d bytes; the page text was not capped", len(stream))
	}

	status, snapshot := do(http.MethodGet, "/api/sessions/local:demo", "")
	if status != http.StatusOK {
		t.Fatalf("snapshot: status %d", status)
	}
	var page2 struct {
		Messages []struct {
			Sender string `json:"sender"`
			Parts  []part `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(snapshot), &page2); err != nil {
		t.Fatal(err)
	}
	var assistant []part
	for _, m := range page2.Messages {
		if m.Sender == "assistant" {
			assistant = m.Parts
		}
	}
	assertFetchPart(t, "session snapshot", assistant, inlineCap, pageBytes)
}

func assertFetchPart(t *testing.T, where string, parts []map[string]any, inlineCap, pageBytes int) {
	t.Helper()
	var fetch, search map[string]any
	hosted := 0
	for _, p := range parts {
		if p["type"] != "hosted_tool" {
			continue
		}
		hosted++
		switch p["name"] {
		case "openrouter:web_fetch":
			fetch = p
		case "web_search":
			search = p
		}
	}
	if hosted != 2 || fetch == nil || search == nil {
		t.Fatalf("%s: want one fetch and one search hosted_tool part, got %d: %v", where, hosted, parts)
	}
	if fetch["callId"] != "st_1" || fetch["status"] != "completed" || fetch["target"] != "https://go.dev/dl/" {
		t.Fatalf("%s: fetch part = %v", where, fetch)
	}
	raw, _ := fetch["raw"].(string)
	if len(raw) == 0 || len(raw) > inlineCap {
		t.Fatalf("%s: raw is %d bytes, want 1..%d", where, len(raw), inlineCap)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(raw), &item); err != nil || item["url"] != "https://go.dev/dl/" || item["httpStatus"].(float64) != 200 {
		t.Fatalf("%s: raw is not the capped item: %v %.200s", where, err, raw)
	}
	truncated, _ := fetch["truncated"].(map[string]any)
	if size, _ := truncated["bytes"].(float64); int(size) < pageBytes {
		t.Fatalf("%s: truncated = %v, want the full size of at least %d", where, fetch["truncated"], pageBytes)
	}
	if search["target"] != "go release" || search["status"] != "completed" {
		t.Fatalf("%s: search part = %v", where, search)
	}
}
