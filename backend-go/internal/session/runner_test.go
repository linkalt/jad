package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/memorytrace"
	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

func TestNormalizeInputAndBuildRequest(t *testing.T) {
	items, err := NormalizeInput(json.RawMessage(`" hello "`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || string(items[0]) != `{"type":"message","role":"user","content":[{"type":"input_text","text":" hello "}]}` {
		t.Fatalf("NormalizeInput(string) = %q", items)
	}

	settings := ModelSettings{
		Model:      "configured-model",
		Request:    json.RawMessage(`{"model":"old-model","stream":false,"temperature":0.4,"future":{"exact_integer":9007199254740993}}`),
		PromptRole: "developer",
		PromptText: "base prompt",
	}
	history := [][]byte{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`)}
	request, err := BuildRequest(settings, history)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields["model"]); got != `"configured-model"` {
		t.Errorf("request model = %s", got)
	}
	if got := string(fields["stream"]); got != "true" {
		t.Errorf("request stream = %s", got)
	}
	if got, want := string(fields["future"]), `{"exact_integer":9007199254740993}`; got != want {
		t.Errorf("future request field = %s, want %s", got, want)
	}
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(fields["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 2 {
		t.Fatalf("request input count = %d, want prompt and history", len(input))
	}
	if got := string(input[0]["role"]); got != `"developer"` {
		t.Errorf("prompt role = %s", got)
	}
	if got := string(input[1]["role"]); got != `"user"` {
		t.Errorf("history role = %s", got)
	}

	for _, raw := range []string{`" \t "`, `[]`, `null`, `42`, `true`} {
		if _, err := NormalizeInput(json.RawMessage(raw)); err == nil {
			t.Errorf("NormalizeInput(%s) succeeded, want error", raw)
		}
	}
}

func TestRunnerStreamsAndPersistsGoldenResponse(t *testing.T) {
	const outputItemEvent = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`
	const completedEvent = `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9,"provider_extra":"discarded"},"output":[],"future_response_field":"retained"}}`

	type requestCapture struct {
		fields map[string]json.RawMessage
		err    error
	}
	requestSeen := make(chan requestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := requestCapture{}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			capture.err = fmt.Errorf("request = %s %s, want POST /v1/responses", r.Method, r.URL.Path)
			requestSeen <- capture
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&capture.fields); err != nil {
			capture.err = fmt.Errorf("decode request: %w", err)
			requestSeen <- capture
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestSeen <- capture
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, payload := range []string{outputItemEvent, completedEvent, "[DONE]"} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	store, journal, stagingDir := newTestStore(t, transcript.ResponseMetadataFull)
	client := openaiapi.NewClient(openaiapi.Config{APIKey: "test-key", BaseURL: server.URL + "/v1"})
	runner, err := NewRunner(store, client, ModelSettings{
		Model:      "gpt-test",
		Request:    json.RawMessage(`{"temperature":0.2,"future_request_option":{"exact_integer":9007199254740993}}`),
		PromptRole: "developer",
		PromptText: "base prompt",
	})
	if err != nil {
		t.Fatal(err)
	}

	var observed []responses.Event
	var traceLines []string
	trace := memorytrace.New(memorytrace.Options{
		Enabled:        true,
		SampleInterval: time.Hour,
		ReadMemory: func() memorytrace.Snapshot {
			return memorytrace.Snapshot{RSSBytes: 7, HeapTotalBytes: 8, HeapUsedBytes: 3}
		},
		WriteLine: func(line string) { traceLines = append(traceLines, line) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, json.RawMessage(`"hello"`), RunOptions{
		TurnID:      "turn-session-run",
		MemoryTrace: trace,
		OnEvent: func(event responses.Event) error {
			observed = append(observed, event)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var tracePhases []string
	var historyTraceBytes float64
	for _, line := range traceLines {
		if !strings.HasPrefix(line, "[provider-memory] ") {
			t.Fatalf("unexpected trace prefix: %q", line)
		}
		if strings.Contains(line, "base prompt") || strings.Contains(line, "hello") {
			t.Fatalf("trace contains prompt or response content: %q", line)
		}
		var record struct {
			Event   string         `json:"event"`
			Phase   string         `json:"phase"`
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "[provider-memory] ")), &record); err != nil {
			t.Fatal(err)
		}
		if record.Event == "phase" {
			tracePhases = append(tracePhases, record.Phase)
			if record.Phase == "history-loaded" {
				historyTraceBytes, _ = record.Details["transcript_bytes"].(float64)
			}
		}
	}
	if got, want := strings.Join(tracePhases, ","), "input-normalized,history-loaded,request-built,provider-response-complete"; got != want {
		t.Fatalf("memory trace phases = %q, want %q", got, want)
	}
	if len(traceLines) != 6 || historyTraceBytes <= 0 {
		t.Fatalf("trace records=%d, history transcript bytes=%v", len(traceLines), historyTraceBytes)
	}
	if result.TurnID != "turn-session-run" {
		t.Errorf("result turn ID = %q", result.TurnID)
	}
	if len(observed) != 2 || string(observed[0].Raw) != outputItemEvent || string(observed[1].Raw) != completedEvent {
		t.Fatalf("observed raw events = %#v", observed)
	}
	if result.Commit.Bytes == 0 || result.Commit.AlreadyPresent {
		t.Errorf("unexpected commit result: %+v", result.Commit)
	}

	captured := <-requestSeen
	if captured.err != nil {
		t.Fatal(captured.err)
	}
	if got := string(captured.fields["model"]); got != `"gpt-test"` {
		t.Errorf("request model = %s", got)
	}
	if got := string(captured.fields["stream"]); got != "true" {
		t.Errorf("request stream = %s", got)
	}
	if got, want := string(captured.fields["future_request_option"]), `{"exact_integer":9007199254740993}`; got != want {
		t.Errorf("future request option = %s, want %s", got, want)
	}
	var requestInput []map[string]json.RawMessage
	if err := json.Unmarshal(captured.fields["input"], &requestInput); err != nil {
		t.Fatal(err)
	}
	if len(requestInput) != 2 || string(requestInput[0]["role"]) != `"developer"` || string(requestInput[1]["role"]) != `"user"` {
		t.Fatalf("request input order = %#v", requestInput)
	}

	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("messages.jsonl record count = %d, want input and response", len(records))
	}
	var persistedInput map[string]json.RawMessage
	if err := json.Unmarshal(records[0], &persistedInput); err != nil {
		t.Fatal(err)
	}
	if got := string(persistedInput[transcript.SessionInputBatchIndexField]); got != "0" {
		t.Errorf("persisted input batch index = %s", got)
	}
	if _, ok := persistedInput[transcript.SessionInputBatchIDField]; !ok {
		t.Error("persisted input is missing batch ID")
	}

	wantLine, err := os.ReadFile(filepath.Join("testdata", "messages-session-response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	wantLine = []byte(strings.TrimSuffix(string(wantLine), "\n"))
	if !reflect.DeepEqual(records[1], wantLine) {
		t.Fatalf("messages.jsonl response differs from Node golden\n got: %s\nwant: %s", records[1], wantLine)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "turn-session-run-0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("committed staging directory still exists: stat err = %v", err)
	}
}

func TestRunnerIncrementsTurnAndNormalizesTerminalEvents(t *testing.T) {
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	streamer := &fakeStreamer{}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}

	streamer.events = []responses.Event{
		{
			Type: "response.output_item.done",
			Raw:  json.RawMessage(`{"type":"response.output_item.done","output_index":"provider-omitted-index","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`),
		},
		{
			Type: "response.completed",
			Raw:  json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`),
		},
	}
	first, err := runner.Run(context.Background(), json.RawMessage(`"first prompt"`), RunOptions{TurnID: "turn-one"})
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	var firstEvent map[string]json.RawMessage
	if err := json.Unmarshal(first.Event, &firstEvent); err != nil {
		t.Fatal(err)
	}
	if got := string(firstEvent["session_turn"]); got != "1" {
		t.Errorf("first session_turn = %s, want 1", got)
	}

	streamer.events = []responses.Event{{
		Type: "response.incomplete",
		Raw:  json.RawMessage(`{"type":"response.incomplete","response":{"id":"resp_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`),
	}}
	second, err := runner.Run(context.Background(), json.RawMessage(`"second prompt"`), RunOptions{TurnID: "turn-two"})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	var secondEvent struct {
		Type        string          `json:"type"`
		SessionTurn int64           `json:"session_turn"`
		SessionStep int64           `json:"session_step"`
		Response    json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(second.Event, &secondEvent); err != nil {
		t.Fatal(err)
	}
	if secondEvent.Type != "response.completed" || secondEvent.SessionTurn != 2 || secondEvent.SessionStep != 1 {
		t.Errorf("second completion envelope = %+v", secondEvent)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(secondEvent.Response, &response); err != nil {
		t.Fatal(err)
	}
	if got := string(response["status"]); got != `"incomplete"` {
		t.Errorf("incomplete response status = %s", got)
	}
	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Errorf("messages.jsonl record count = %d, want two input and two response records", len(records))
	}
}

func TestRunnerLeavesFailedStreamStagedForRecovery(t *testing.T) {
	store, journal, stagingDir := newTestStore(t, transcript.ResponseMetadataMinimal)
	streamFailure := errors.New("provider stream ended unexpectedly")
	streamer := &fakeStreamer{
		events: []responses.Event{{
			Type: "response.output_item.done",
			Raw:  json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`),
		}},
		err: streamFailure,
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"hello"`), RunOptions{TurnID: "turn-failed"})
	if !errors.Is(err, streamFailure) {
		t.Fatalf("Run() error = %v, want provider stream failure", err)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "turn-failed-0", "manifest.json")); err != nil {
		t.Fatalf("incomplete turn manifest was not retained: %v", err)
	}
	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("messages.jsonl records = %d, want only persisted input", len(records))
	}
}

type fakeStreamer struct {
	events []responses.Event
	err    error
}

func (s *fakeStreamer) Stream(_ context.Context, _ json.RawMessage, handle responses.EventHandler) error {
	for _, event := range s.events {
		if err := handle(event); err != nil {
			return err
		}
	}
	return s.err
}

func newTestStore(t *testing.T, metadata transcript.ResponseMetadataMode) (*transcript.TurnStore, *transcript.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	stagingDir := filepath.Join(dir, "staging")
	store, err := transcript.NewTurnStore(journal, stagingDir, transcript.TurnStoreOptions{
		MaxItemBytes:     1 << 20,
		ResponseMetadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, journal, stagingDir
}
