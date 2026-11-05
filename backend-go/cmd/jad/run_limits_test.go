package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs/layouttest"
)

const limitAnswer = "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7},\"output\":[]}}\n\ndata: [DONE]\n\n"

func limitFixture(t *testing.T, provider *httptest.Server, settings map[string]any) string {
	t.Helper()
	fixture := useFixture(t, layouttest.Spec{
		Models:   map[string]map[string]any{"openai/default": {"provider": "openai", "model": "m", "api_key_env": "FIXTURE_KEY", "base_url": provider.URL + "/v1"}},
		Server:   map[string]any{"log_level": "off"},
		Settings: settings,
	})
	t.Setenv("FIXTURE_KEY", "k")
	return "local:" + fixture.SessionID
}

// max_run_duration_ms stops a run that is still going, here a provider that sent headers
// and then nothing, long before the idle guard would; the run fails naming the limit.
func TestRunStopsAtMaxRunDuration(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(provider.Close)
	session := limitFixture(t, provider, map[string]any{"max_run_duration_ms": 1000})

	started := time.Now()
	stdout, stderr, err := runJad(t, "", "run", "--session", session, "--format", "json", "--timeout", "30s", "hello")
	elapsed := time.Since(started)
	var result headless.Result
	if json.Unmarshal([]byte(stdout), &result) != nil {
		t.Fatalf("output %q %q %v", stdout, stderr, err)
	}
	if result.Status != headless.StatusFailed || result.Error != "The run was stopped: it ran for the max_run_duration_ms limit of 1s." {
		t.Fatalf("result = %+v", result)
	}
	if elapsed < time.Second || elapsed > 15*time.Second {
		t.Fatalf("the run took %s", elapsed)
	}
}

// provider_stream.header_timeout_ms ends a request whose headers do not come, and the retry
// rules send it again.
func TestRunRetriesARequestPastTheHeaderTimeout(t *testing.T) {
	var calls atomic.Int32
	released := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-released:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, limitAnswer)
	}))
	t.Cleanup(provider.Close)
	// Cleanups run last first: the held request is let go before the server closes.
	t.Cleanup(func() { close(released) })
	session := limitFixture(t, provider, map[string]any{
		"provider_stream": map[string]any{"header_timeout_ms": 1000},
		"provider_retry":  map[string]any{"initial_delay_ms": 10, "max_delay_ms": 10},
	})

	started := time.Now()
	stdout, stderr, err := runJad(t, "", "run", "--session", session, "--timeout", "30s", "hello")
	if err != nil || strings.TrimSpace(stdout) != "done" {
		t.Fatalf("run: %q %q %v", stdout, stderr, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("the retried run took %s", elapsed)
	}
}
