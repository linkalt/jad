package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryRefusesARunBeyondTheCapAndFreesTheSlotWhenARunEnds(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096, 2)
	first, err := registry.begin("a\x00", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.begin("b\x00", "t2"); err != nil {
		t.Fatal(err)
	}
	_, err = registry.begin("c\x00", "t3")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusTooManyRequests || statusErr.RetryAfterSeconds < 1 {
		t.Fatalf("third begin error = %#v, want 429 with a Retry-After", err)
	}
	first.finish("completed", nil)
	if _, err := registry.begin("c\x00", "t3"); err != nil {
		t.Fatalf("a finished run must free its slot, got %v", err)
	}
}

func TestRegistryWithoutACapNeverRefuses(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096, 0)
	for index := range 50 {
		if _, err := registry.begin("k\x00"+strings.Repeat("x", index), "t"); err != nil {
			t.Fatalf("run %d refused with no cap: %v", index, err)
		}
	}
}

func TestRunCapAnswersWith429AndRetryAfter(t *testing.T) {
	resolver, _ := newIdleTranscriptResolver(t)
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxConcurrentRuns: 1})
	defer handler.Close()
	if _, err := handler.beginRun("one\x00", "t1"); err != nil {
		t.Fatal(err)
	}
	_, err := handler.beginRun("two\x00", "t2")
	if err == nil {
		t.Fatal("second run was accepted beyond the cap")
	}
	response := httptest.NewRecorder()
	writeJSONError(response, err)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(response.Body.String(), "server.max_concurrent_runs") {
		t.Fatalf("body = %s, want the setting named", response.Body.String())
	}
}

func TestNewHandlerRejectsAnOutOfRangeRunCap(t *testing.T) {
	resolver, _ := newIdleTranscriptResolver(t)
	for _, value := range []int{-1, 1025} {
		_, err := NewHandler(Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, SnapshotTurns: 24, MaxPageTurns: 50, MaxConcurrentRuns: value}, resolver)
		if err == nil {
			t.Fatalf("MaxConcurrentRuns %d was accepted", value)
		}
	}
}

func TestRunStartAtTheCapIsRefusedOverHTTPAndReleasesTheTranscript(t *testing.T) {
	resolver, _ := newIdleTranscriptResolver(t)
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxConcurrentRuns: 1})
	defer handler.Close()
	if _, err := handler.beginRun("other\x00", "t1"); err != nil {
		t.Fatal(err)
	}
	response := postRun(handler)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if held := len(resolver.transcript); held != 0 {
		t.Fatalf("transcript lock still held after a refused run (%d)", held)
	}
}
