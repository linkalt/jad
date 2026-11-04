package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/sessionfs"
)

// fakeStreamer answers with err, after sending events first.
type fakeStreamer struct {
	mu     sync.Mutex
	bodies []string
	events int
	err    error
}

func (f *fakeStreamer) Stream(_ context.Context, body json.RawMessage, handle responses.EventHandler) error {
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	events, err := f.events, f.err
	f.mu.Unlock()
	for range events {
		if handleErr := handle(responses.Event{Type: "response.created", Raw: json.RawMessage(`{}`)}); handleErr != nil {
			return handleErr
		}
	}
	return err
}

func (f *fakeStreamer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// statusError builds the error the provider client returns for an HTTP status.
func statusError(t *testing.T, status int) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()
	client := openaiapi.NewClient(openaiapi.Config{APIKey: "k", BaseURL: server.URL, Retry: &openaiapi.RetryConfig{MaxRetries: 0}})
	err := client.Stream(context.Background(), json.RawMessage(`{"model":"m"}`), func(responses.Event) error { return nil })
	if providerStatus(err) != status {
		t.Fatalf("client error %v has status %d, want %d", err, providerStatus(err), status)
	}
	return err
}

func newFallback(primary responses.Streamer, settings sessionfs.ModelFallbackSettings, targets ...*fakeStreamer) (*fallbackStreamer, *time.Time) {
	now := time.Unix(1000, 0)
	streamer := &fallbackStreamer{primary: primary, shape: modelShape{name: "primary"}, settings: settings, logger: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}
	for index, target := range targets {
		streamer.targets = append(streamer.targets, fallbackTarget{id: fmt.Sprintf("fb/%d", index), shape: modelShape{name: fmt.Sprintf("fallback-%d", index)}, streamer: target})
	}
	return streamer, &now
}

var fallbackDefaults = sessionfs.ModelFallbackSettings{OnStatus: []int{429, 503}, OnNetworkError: true, CooldownMS: 60_000}

func TestFallbackStreamerTriesTheNextModelOnAListedStatus(t *testing.T) {
	overloaded := statusError(t, 503)
	primary, first, second := &fakeStreamer{err: overloaded}, &fakeStreamer{err: overloaded}, &fakeStreamer{}
	streamer, now := newFallback(primary, fallbackDefaults, first, second)
	if err := streamer.Stream(context.Background(), json.RawMessage(`{"model":"primary","input":[]}`), func(responses.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if primary.calls() != 1 || first.calls() != 1 || second.calls() != 1 || !strings.Contains(second.bodies[0], `"model":"fallback-1"`) {
		t.Fatalf("calls %d %d %d, body %v", primary.calls(), first.calls(), second.calls(), second.bodies)
	}
	// Within the cooldown the session's model is skipped.
	if err := streamer.Stream(context.Background(), json.RawMessage(`{"model":"primary"}`), func(responses.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if primary.calls() != 1 || first.calls() != 2 {
		t.Fatalf("during the cooldown: primary %d, first %d", primary.calls(), first.calls())
	}
	// After it, the session's model is asked first again.
	*now = now.Add(61 * time.Second)
	primary.err = nil
	if err := streamer.Stream(context.Background(), json.RawMessage(`{"model":"primary"}`), func(responses.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if primary.calls() != 2 || first.calls() != 2 {
		t.Fatalf("after the cooldown: primary %d, first %d", primary.calls(), first.calls())
	}
}

func TestFallbackStreamerKeepsFailuresItMustNotRetryElsewhere(t *testing.T) {
	badRequest := statusError(t, 400)
	cases := map[string]struct {
		primary *fakeStreamer
		ctx     func() context.Context
	}{
		"status not listed":   {&fakeStreamer{err: badRequest}, context.Background},
		"after an event":      {&fakeStreamer{err: statusError(t, 503), events: 1}, context.Background},
		"network after event": {&fakeStreamer{err: errors.New("connection reset"), events: 1}, context.Background},
		"stopped run": {&fakeStreamer{err: context.Canceled}, func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for name, test := range cases {
		fallback := &fakeStreamer{}
		streamer, _ := newFallback(test.primary, fallbackDefaults, fallback)
		err := streamer.Stream(test.ctx(), json.RawMessage(`{"model":"primary"}`), func(responses.Event) error { return nil })
		if err == nil || fallback.calls() != 0 {
			t.Errorf("%s: err %v, fallback calls %d", name, err, fallback.calls())
		}
	}
}

func TestFallbackStreamerNetworkErrorsAndTheLastFailure(t *testing.T) {
	refused := errors.New("dial tcp: connection refused")
	fallback := &fakeStreamer{}
	streamer, _ := newFallback(&fakeStreamer{err: refused}, fallbackDefaults, fallback)
	if err := streamer.Stream(context.Background(), json.RawMessage(`{}`), func(responses.Event) error { return nil }); err != nil || fallback.calls() != 1 {
		t.Fatalf("network error: err %v, fallback calls %d", err, fallback.calls())
	}
	off := fallbackDefaults
	off.OnNetworkError = false
	fallback = &fakeStreamer{}
	streamer, _ = newFallback(&fakeStreamer{err: refused}, off, fallback)
	if err := streamer.Stream(context.Background(), json.RawMessage(`{}`), func(responses.Event) error { return nil }); !errors.Is(err, refused) || fallback.calls() != 0 {
		t.Fatalf("on_network_error false: err %v, fallback calls %d", err, fallback.calls())
	}
	// Every model failing returns the last failure; in the cooldown the session's model is
	// asked after the fallbacks.
	limited := statusError(t, 429)
	primary := &fakeStreamer{err: statusError(t, 503)}
	fallback = &fakeStreamer{err: limited}
	streamer, _ = newFallback(primary, fallbackDefaults, fallback)
	if err := streamer.Stream(context.Background(), json.RawMessage(`{}`), func(responses.Event) error { return nil }); providerStatus(err) != 429 {
		t.Fatalf("all failed: %v", err)
	}
	if err := streamer.Stream(context.Background(), json.RawMessage(`{}`), func(responses.Event) error { return nil }); providerStatus(err) != 503 || primary.calls() != 2 || fallback.calls() != 2 {
		t.Fatalf("cooldown, all failed: %v, primary %d, fallback %d", err, primary.calls(), fallback.calls())
	}
}

func TestRewriteForModel(t *testing.T) {
	from := modelShape{name: "big", request: json.RawMessage(`{"reasoning":{"effort":"high"},"store":false}`), hostedTools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}}
	to := modelShape{name: "small", request: json.RawMessage(`{"max_output_tokens":100,"model":"ignored"}`)}
	body := `{"input":[{"role":"user"}],"model":"big","reasoning":{"effort":"high"},"store":false,"stream":true,"tools":[{"type":"function","name":"read"},{"type": "web_search"}]}`
	out, err := rewriteForModel(json.RawMessage(body), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"input":[{"role":"user"}],"max_output_tokens":100,"model":"small","stream":true,"tools":[{"type":"function","name":"read"}]}`; string(out) != want {
		t.Fatalf("rewritten:\n%s\nwant\n%s", out, want)
	}
	// The other way round the hosted tools come back, and a body without tools gets them.
	back, err := rewriteForModel(out, to, from)
	if err != nil || !strings.Contains(string(back), `"tools":[{"type":"function","name":"read"},{"type":"web_search"}]`) || !strings.Contains(string(back), `"reasoning":{"effort":"high"}`) {
		t.Fatalf("back: %s %v", back, err)
	}
	alone, _ := rewriteForModel(json.RawMessage(`{"model":"small"}`), to, from)
	if !strings.Contains(string(alone), `"tools":[{"type":"web_search"}]`) {
		t.Fatalf("no tools: %s", alone)
	}
	if _, err := rewriteForModel(json.RawMessage(`[]`), from, to); err == nil {
		t.Fatal("a non-object body was rewritten")
	}
}

// A headless run whose model answers 503 is answered by the fallback model of another
// server, and a fallback whose key is missing is left out.
func TestModelFallbackAnswersAHeadlessRun(t *testing.T) {
	var primaryCalls, fallbackCalls int
	var mu sync.Mutex
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		primaryCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	}))
	t.Cleanup(primary.Close)
	var fallbackBody string
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		fallbackCalls++
		fallbackBody = string(data)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"from the fallback"}]}}`,
			`{"type":"response.completed","response":{"id":"r1","status":"completed","model":"spare","output":[]}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	t.Cleanup(fallback.Close)

	f := newLayoutFixture(t)
	files := map[string]any{
		"models/openai/small.json": map[string]any{"provider": "openai", "model": "model-small", "api_key_env": "FIXTURE_KEY", "base_url": primary.URL + "/v1"},
		"models/spare/one.json":    map[string]any{"provider": "openai", "model": "spare", "api_key_env": "FIXTURE_KEY", "base_url": fallback.URL + "/v1", "request": map[string]any{"max_output_tokens": 77}},
		"models/spare/nokey.json":  map[string]any{"provider": "openai", "model": "nokey", "api_key_env": "MISSING_KEY", "base_url": fallback.URL + "/v1"},
		"profiles/dev/coder.json": map[string]any{"models": []string{"openai/small", "openai/large"}, "model_default": "openai/small", "toolset": "coding/none",
			"settings": map[string]any{"provider_retry": map[string]any{"max_retries": 0}, "model_fallback": map[string]any{"models": []string{"spare/nokey", "spare/one"}}}},
	}
	for rel, value := range files {
		data, _ := json.Marshal(value)
		path := filepath.Join(f.root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := headlessFor(t, f).Run(context.Background(), headless.Request{Session: "local:demo", Profile: "dev/coder", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != headless.StatusCompleted || result.Text != "from the fallback" {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if primaryCalls != 1 || fallbackCalls != 1 || !strings.Contains(fallbackBody, `"model":"spare"`) || !strings.Contains(fallbackBody, `"max_output_tokens":77`) || !strings.Contains(fallbackBody, `"text":"hi"`) {
		t.Fatalf("primary %d, fallback %d, body %s", primaryCalls, fallbackCalls, fallbackBody)
	}
}
