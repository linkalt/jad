package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/transcript"
)

func TestHTTPRunRouteWithRealSessionRunnerAndLocalResponsesFixture(t *testing.T) {
	const outputItem = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`
	const delta1 = `{"type":"response.output_text.delta","delta":"hel"}`
	const delta2 = `{"type":"response.output_text.delta","delta":"lo"}`
	const completed = `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9,"provider_extra":"discarded"},"output":[],"future_response_field":"retained"}}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range []string{delta1, delta2, outputItem, completed, "[DONE]"} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
		}
	}))
	defer provider.Close()

	dir := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(dir, "staging"), transcript.TurnStoreOptions{
		MaxItemBytes:     1 << 20,
		ResponseMetadata: transcript.ResponseMetadataFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := openaiapi.NewClient(openaiapi.Config{APIKey: "local-test-key", BaseURL: provider.URL + "/v1"})
	runner, err := session.NewRunner(store, client, session.ModelSettings{
		Model:   "gpt-test",
		Request: json.RawMessage(`{"temperature":0.2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "session-1", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken:            testAPIToken,
		MaxBodyBytes:        4096,
		SSEMaxBufferedBytes: 4096,
		RunStateRetention:   time.Minute,
	})
	defer handler.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-1/run", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("run route status = %d, body=%s", response.Code, response.Body.String())
	}
	events := parseEvents(t, response.Body.String())
	if got, want := eventNames(events), []string{"run", "ui.message", "ui.delta", "ui.message", "completed", "done"}; !equalStrings(got, want) {
		t.Fatalf("run SSE events = %v, want %v", got, want)
	}
	var final struct {
		Message webUIMessage `json:"message"`
	}
	if err := json.Unmarshal(events[3].data, &final); err != nil {
		t.Fatal(err)
	}
	if final.Message.Text != "hello" || final.Message.Status != "complete" {
		t.Fatalf("final Web UI message = %+v", final.Message)
	}

	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("messages.jsonl record count = %d, want input and committed response", len(records))
	}
	golden, err := os.ReadFile(filepath.Join("..", "session", "testdata", "messages-session-response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(records[1]), strings.TrimSuffix(string(golden), "\n"); got != want {
		t.Fatalf("HTTP run response line differs from Node golden\n got: %s\nwant: %s", got, want)
	}

	attach := httptest.NewRequest(http.MethodGet, "/api/sessions/session-1/run/stream", nil)
	attach.Header.Set("Authorization", "Bearer "+testAPIToken)
	attached := httptest.NewRecorder()
	handler.ServeHTTP(attached, attach)
	if attached.Code != http.StatusOK {
		t.Fatalf("run attach failed: %d %s", attached.Code, attached.Body.String())
	}
	if got, want := eventNames(parseEvents(t, attached.Body.String())), []string{"run", "ui.message", "completed", "done"}; !equalStrings(got, want) {
		t.Fatalf("attached SSE events = %v, want %v", got, want)
	}
}
