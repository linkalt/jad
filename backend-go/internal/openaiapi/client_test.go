package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
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

// failingServer answers every request with the given status and body.
func failingServer(t *testing.T, status int, contentType, body string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return NewClient(Config{APIKey: "test-api-key", BaseURL: server.URL + "/v1"})
}

func streamError(t *testing.T, client *Client) error {
	t.Helper()
	err := client.Stream(context.Background(), json.RawMessage(`{"model":"m","input":"hi"}`), func(Event) error { return nil })
	if err == nil {
		t.Fatal("Stream() succeeded, want an error")
	}
	return err
}

func TestStreamErrorCarriesABodyThatIsNotInTheOpenAIShape(t *testing.T) {
	cases := []struct {
		name, contentType, body, want string
	}{
		{"detail object", "application/json", `{"detail":"Store must be set to false"}`, `{"detail":"Store must be set to false"}`},
		{"pretty detail object", "application/json", "{\n  \"detail\": \"Store must be set to false\"\n}\n", `{"detail":"Store must be set to false"}`},
		{"plain text", "text/plain", "upstream gateway says no\n", "upstream gateway says no"},
		{"html", "text/html", "<html>Forbidden</html>", "<html>Forbidden</html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := streamError(t, failingServer(t, http.StatusBadRequest, tc.contentType, tc.body))
			if !strings.HasSuffix(err.Error(), "400 Bad Request "+tc.want) {
				t.Errorf("error = %q, want it to end with %q", err.Error(), "400 Bad Request "+tc.want)
			}
			var status *StatusError
			if !errors.As(err, &status) || status.ProviderStatus() != http.StatusBadRequest {
				t.Errorf("errors.As(StatusError) = %v, status %v, want 400", status != nil, status)
			}
			if statusOf(err) != http.StatusBadRequest {
				t.Errorf("statusOf = %d, want 400: the SDK error must stay reachable", statusOf(err))
			}
		})
	}
}

func TestStreamErrorInTheOpenAIShapeIsNotChanged(t *testing.T) {
	body := `{"error":{"message":"Bad model","type":"invalid_request_error","param":null,"code":"model_not_found"}}`
	err := streamError(t, failingServer(t, http.StatusBadRequest, "application/json", body))
	got := err.Error()
	if !strings.HasSuffix(got, `"code":"model_not_found"}`) || strings.Count(got, "Bad model") != 1 {
		t.Errorf("error = %q, want the SDK's text with the error object once", got)
	}
}

func TestStreamErrorWithAnEmptyBodyStaysAsBefore(t *testing.T) {
	err := streamError(t, failingServer(t, http.StatusBadRequest, "", ""))
	if !strings.HasSuffix(err.Error(), "400 Bad Request ") {
		t.Errorf("error = %q, want the unchanged SDK text", err.Error())
	}
}
