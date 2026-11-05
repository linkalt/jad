package headless

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token"

// stubHandler answers the run route with frames and records the stop route.
type stubHandler struct {
	mu      sync.Mutex
	status  int
	frames  []string
	stopped []string
	// hold keeps the stream open after the frames until the stop route is called, then
	// writes the stop frames.
	hold      bool
	stopCh    chan struct{}
	stopOnce  sync.Once
	afterStop []string
	paths     []string
}

func (s *stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.RequestURI())
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/run/stop") {
		s.mu.Lock()
		s.stopped = append(s.stopped, r.URL.RequestURI())
		s.mu.Unlock()
		s.stopOnce.Do(func() { close(s.stopCh) })
		return
	}
	if s.status != 0 && s.status != http.StatusOK {
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"error":"session not found"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	for _, frame := range s.frames {
		_, _ = io.WriteString(w, frame)
		w.(http.Flusher).Flush()
	}
	if s.hold {
		<-s.stopCh
		for _, frame := range s.afterStop {
			_, _ = io.WriteString(w, frame)
		}
	}
}

func newStub(frames ...string) *stubHandler {
	return &stubHandler{frames: frames, stopCh: make(chan struct{})}
}

func frame(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

func TestRunReadsTheStreamAndSkipsKeepAlives(t *testing.T) {
	stub := newStub(
		frame("run", `{"turn_id":"t-1","status":"running"}`),
		": keep-alive\n\n",
		frame("ui.message", `{"message":{"text":"partial"}}`),
		frame("usage", `{"usage":{"input_tokens":3}}`),
		frame("completed", `{"message":{"text":"final answer"},"committed":true}`),
		frame("done", `{}`),
	)
	var events []Event
	result, err := NewRunner(stub, testToken).Run(context.Background(), Request{
		Session: "local:demo", Profile: "dev/coder", Input: "hi",
		OnEvent: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Text != "final answer" || result.TurnID != "t-1" || string(result.Usage) != `{"input_tokens":3}` {
		t.Fatalf("result = %+v", result)
	}
	if len(events) != 5 || events[0].Name != "run" || events[4].Name != "done" {
		t.Fatalf("events = %+v, want 5 without the keep-alive", events)
	}
	if got := stub.paths[0]; got != "POST /api/sessions/local:demo/run?profile=dev%2Fcoder" {
		t.Fatalf("path = %q", got)
	}
}

func TestRunReportsAFailedRun(t *testing.T) {
	stub := newStub(
		frame("ui.message", `{"message":{"text":"so far"}}`),
		frame("error", `{"message":"The provider request failed (HTTP 500)."}`),
		frame("done", `{}`),
	)
	result, err := NewRunner(stub, testToken).Run(context.Background(), Request{Session: "s", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusFailed || result.Error == "" || result.Text != "so far" {
		t.Fatalf("result = %+v, want failed with the message and the text so far", result)
	}
}

// The report of stop commands skipped at a cap reaches the result; other hook reports,
// a stop command's own among them, do not.
func TestRunKeepsTheReportOfSkippedStopHooks(t *testing.T) {
	stub := newStub(
		frame("hook_report", `{"turn_id":"t","event":"stop","trigger":"","report":"[verify] passed (2 ms, in .)"}`),
		frame("hook_report", `{"turn_id":"t","event":"run_end","trigger":"cap_reached","report":"[audit] passed"}`),
		frame("hook_report", `{"turn_id":"t","event":"stop","trigger":"cap_reached","report":"[stop hooks] not run: max_run_tokens reached"}`),
		frame("completed", `{"message":{"text":"done"}}`),
		frame("done", `{}`),
	)
	result, err := NewRunner(stub, testToken).Run(context.Background(), Request{Session: "s", Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.StopHooksSkipped != "[stop hooks] not run: max_run_tokens reached" {
		t.Fatalf("result = %+v, want completed with the skip report", result)
	}
	encoded, _ := json.Marshal(Result{Status: StatusCompleted})
	if strings.Contains(string(encoded), "stop_hooks_skipped") {
		t.Fatalf("a run without the report encodes it: %s", encoded)
	}
}

func TestRunStreamWithoutAnEndIsAFailure(t *testing.T) {
	for name, frames := range map[string][]string{
		"no done":          {frame("run", `{"turn_id":"t"}`)},
		"done, no outcome": {frame("done", `{}`)},
		"cut mid-frame":    {"event: completed\ndata: {"},
		"nothing at all":   nil,
	} {
		result, err := NewRunner(newStub(frames...), testToken).Run(context.Background(), Request{Session: "s", Input: "hi"})
		if err != nil || result.Status != StatusFailed {
			t.Errorf("%s: result = %+v, err = %v, want failed", name, result, err)
		}
	}
}

func TestRunRefusedRequestIsAnError(t *testing.T) {
	stub := newStub()
	stub.status = http.StatusNotFound
	_, err := NewRunner(stub, testToken).Run(context.Background(), Request{Session: "s", Input: "hi"})
	var refused *RequestError
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound || refused.Message != "session not found" {
		t.Fatalf("error = %v, want a 404 request error with the message", err)
	}
	if _, err := NewRunner(stub, "wrong").Run(context.Background(), Request{Session: "s", Input: "hi"}); !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token: error = %v, want 401", err)
	}
}

func TestRunRefusesAnEmptyPromptOrSession(t *testing.T) {
	runner := NewRunner(newStub(), testToken)
	if _, err := runner.Run(context.Background(), Request{Session: "s", Input: "  "}); err == nil {
		t.Fatal("empty prompt: no error")
	}
	if _, err := runner.Run(context.Background(), Request{Input: "hi"}); err == nil {
		t.Fatal("empty session: no error")
	}
}

func TestRunTimeoutAndCancelStopTheRunOnce(t *testing.T) {
	stopFrames := []string{frame("error", `{"message":"The run was stopped before it completed."}`), frame("done", `{}`)}
	t.Run("timeout", func(t *testing.T) {
		stub := newStub(frame("run", `{"turn_id":"t"}`))
		stub.hold, stub.afterStop = true, stopFrames
		result, err := NewRunner(stub, testToken).Run(context.Background(), Request{Session: "s", Profile: "p/q", Input: "hi", Timeout: 20 * time.Millisecond})
		if err != nil || result.Status != StatusTimedOut || result.Error == "" {
			t.Fatalf("result = %+v, err = %v, want timed_out", result, err)
		}
		if len(stub.stopped) != 1 || stub.stopped[0] != "/api/sessions/s/run/stop?profile=p%2Fq" {
			t.Fatalf("stop calls = %v", stub.stopped)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		stub := newStub(frame("run", `{"turn_id":"t"}`))
		stub.hold, stub.afterStop = true, stopFrames
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := NewRunner(stub, testToken).Run(ctx, Request{Session: "s", Input: "hi", Timeout: time.Hour})
		if err != nil || result.Status != StatusAborted {
			t.Fatalf("result = %+v, err = %v, want aborted", result, err)
		}
		if len(stub.stopped) != 1 {
			t.Fatalf("stop calls = %v, want one", stub.stopped)
		}
	})
}
