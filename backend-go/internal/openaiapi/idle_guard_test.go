package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const idleTestRequest = `{"model":"test-model","input":[]}`

// silentProvider sends the headers and each event in events, then holds the
// connection open without sending anything until the test ends.
func silentProvider(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, payload := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

const idleTestEvent = `{"type":"response.output_text.delta","delta":"hi","item_id":"i","output_index":0,"content_index":0,"sequence_number":1}`

func TestStreamFailsWhenTheProviderGoesSilent(t *testing.T) {
	server := silentProvider(t, idleTestEvent)
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", IdleTimeout: 150 * time.Millisecond})
	var seen atomic.Int64
	started := time.Now()
	err := client.Stream(context.Background(), json.RawMessage(idleTestRequest), func(Event) error {
		seen.Add(1)
		return nil
	})
	var idle *StreamIdleError
	if !errors.As(err, &idle) {
		t.Fatalf("error = %v, want a StreamIdleError", err)
	}
	if idle.Timeout != 150*time.Millisecond {
		t.Fatalf("timeout = %s, want 150ms", idle.Timeout)
	}
	if seen.Load() != 1 {
		t.Fatalf("events seen = %d, want the one sent before the silence", seen.Load())
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("took %s, want about the idle timeout", elapsed)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error %v must not look like a user stop", err)
	}
}

func TestStreamFailsWhenTheProviderNeverSendsAnEvent(t *testing.T) {
	server := silentProvider(t)
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", IdleTimeout: 150 * time.Millisecond})
	err := client.Stream(context.Background(), json.RawMessage(idleTestRequest), func(Event) error { return nil })
	var idle *StreamIdleError
	if !errors.As(err, &idle) {
		t.Fatalf("error = %v, want a StreamIdleError", err)
	}
}

func TestStreamIdleTimeoutDoesNotCountTheHandlersTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Each event is flushed on its own and the second comes after the client's
		// idle time has passed, so a guard still armed during the handler would have
		// cancelled the body before the second event can be read.
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", idleTestEvent)
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", idleTestEvent)
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", IdleTimeout: 100 * time.Millisecond})
	var seen int
	err := client.Stream(context.Background(), json.RawMessage(idleTestRequest), func(Event) error {
		seen++
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("a slow handler must not trip the guard: %v", err)
	}
	if seen != 2 {
		t.Fatalf("events seen = %d, want 2", seen)
	}
}

func TestStreamStopByTheCallerIsNotReportedAsIdle(t *testing.T) {
	server := silentProvider(t, idleTestEvent)
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", IdleTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	err := client.Stream(ctx, json.RawMessage(idleTestRequest), func(Event) error {
		cancel()
		return nil
	})
	var idle *StreamIdleError
	if errors.As(err, &idle) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a plain cancellation", err)
	}
}

func TestStreamWithoutAnIdleTimeoutKeepsWaiting(t *testing.T) {
	server := silentProvider(t, idleTestEvent)
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := client.Stream(ctx, json.RawMessage(idleTestRequest), func(Event) error { return nil })
	var idle *StreamIdleError
	if errors.As(err, &idle) || err == nil {
		t.Fatalf("error = %v, want the caller's own deadline, not an idle error", err)
	}
}

// A retry wait is the client's own backoff, not provider silence: with a wait longer
// than the idle timeout the second attempt must still run and succeed.
func TestStreamIdleGuardIsQuietWhileARetryWaits(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", idleTestEvent)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client := NewClient(Config{
		APIKey: "k", BaseURL: server.URL + "/v1/", IdleTimeout: 100 * time.Millisecond,
		Retry: &RetryConfig{MaxRetries: 1, InitialDelay: time.Millisecond, MaxDelay: time.Second},
	})
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var seen int
	err := client.Stream(context.Background(), json.RawMessage(idleTestRequest), func(Event) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("a retry wait longer than the idle timeout failed the call: %v", err)
	}
	if requests.Load() != 2 || seen != 1 {
		t.Fatalf("requests = %d, events = %d, want 2 and 1", requests.Load(), seen)
	}
}

func TestStreamUserStopWhileTheGuardIsArmedIsAStop(t *testing.T) {
	server := silentProvider(t)
	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/v1", IdleTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := client.Stream(ctx, json.RawMessage(idleTestRequest), func(Event) error { return nil })
	var idle *StreamIdleError
	if errors.As(err, &idle) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a plain cancellation, not an idle error", err)
	}
}
