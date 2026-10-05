package openaiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStreamPreservesRawRequestAndResponseEvents(t *testing.T) {
	requestFutureValue := json.RawMessage(`{"exact_integer":9007199254740993}`)
	type requestCapture struct {
		fields map[string]json.RawMessage
		err    error
	}
	requestSeen := make(chan requestCapture, 1)

	outputItemEvent := `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}","future_item_field":"preserved"},"future_event_field":{"exact_integer":9007199254740993}}`
	completedEvent := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"test-model","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}","future_output_field":"preserved"}],"future_response_field":"preserved"},"future_event_field":"preserved"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := requestCapture{}
		if r.Method != http.MethodPost {
			capture.err = fmt.Errorf("method = %q, want POST", r.Method)
			requestSeen <- capture
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/v1/responses" {
			capture.err = fmt.Errorf("path = %q, want /v1/responses", r.URL.Path)
			requestSeen <- capture
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-api-key" {
			capture.err = fmt.Errorf("Authorization = %q, want Bearer test-api-key", got)
			requestSeen <- capture
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&capture.fields); err != nil {
			capture.err = fmt.Errorf("decode request body: %w", err)
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

	client := NewClient(Config{APIKey: "test-api-key", BaseURL: server.URL + "/v1"})
	request := json.RawMessage(`{"model":"test-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"future_request_option":{"exact_integer":9007199254740993}}`)

	var events []Event
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Stream(ctx, request, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	captured := <-requestSeen
	if captured.err != nil {
		t.Fatal(captured.err)
	}

	if got := string(captured.fields["model"]); got != `"test-model"` {
		t.Errorf("request model = %s, want %q", got, "test-model")
	}
	if got, want := string(captured.fields["future_request_option"]), string(requestFutureValue); got != want {
		t.Errorf("future request field = %s, want %s", got, want)
	}
	if got := string(captured.fields["stream"]); got != "true" {
		t.Errorf("request stream = %s, want true", got)
	}

	if len(events) != 2 {
		t.Fatalf("received %d events, want 2", len(events))
	}
	wantEvents := []Event{
		{Type: "response.output_item.done", Raw: json.RawMessage(outputItemEvent)},
		{Type: "response.completed", Raw: json.RawMessage(completedEvent)},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events differ from original SSE payloads\n got: %#v\nwant: %#v", events, wantEvents)
	}
	for _, event := range events {
		if !json.Valid(event.Raw) {
			t.Errorf("event %q is not valid JSON: %s", event.Type, event.Raw)
		}
	}
}

func TestStreamRejectsNonObjectRequestBeforeNetwork(t *testing.T) {
	client := NewClient(Config{APIKey: "test-api-key", BaseURL: "http://127.0.0.1:1/v1"})
	for _, request := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{not-json}`)} {
		err := client.Stream(context.Background(), request, func(Event) error { return nil })
		if err == nil {
			t.Errorf("Stream(%s) succeeded, want request validation error", request)
			continue
		}
		if strings.Contains(err.Error(), "connection refused") {
			t.Errorf("Stream(%s) made a network request before validation: %v", request, err)
		}
	}
}

func TestStreamPropagatesHandlerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewClient(Config{APIKey: "test-api-key", BaseURL: server.URL + "/v1"})
	wantErr := fmt.Errorf("consumer stopped")
	err := client.Stream(context.Background(), json.RawMessage(`{"model":"test-model","input":"hello"}`), func(Event) error {
		return wantErr
	})
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Stream() error = %v, want wrapped %q", err, wantErr)
	}
}
