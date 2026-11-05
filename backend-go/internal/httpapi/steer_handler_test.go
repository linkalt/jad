package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

func postSteer(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/session-steer/run/steer", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestRunSteerQueuesAMessageForTheRunningRun(t *testing.T) {
	started := make(chan *session.SteerQueue, 1)
	release := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		started <- options.Steer
		select {
		case <-release:
		case <-ctx.Done():
		}
		options.Steer.Close()
		return session.RunResult{}, ctx.Err()
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-steer", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, SteerMaxPending: 1,
	})
	defer handler.Close()

	if response := postSteer(t, handler, `{"input":"hi"}`); response.Code != http.StatusNotFound {
		t.Fatalf("steer without a run = %d %s, want 404", response.Code, response.Body.String())
	}
	start := httptest.NewRequest(http.MethodPost, "/api/sessions/session-steer/run", strings.NewReader(`{"input":"hello"}`))
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	go handler.ServeHTTP(httptest.NewRecorder(), start)
	var queue *session.SteerQueue
	select {
	case queue = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not start")
	}
	if queue == nil {
		t.Fatal("the run got no steering queue")
	}
	for _, bad := range []string{`{}`, `{"input":null}`, `{"input":""}`, `{"input":"x","extra":1}`, `{"input":42}`, `not json`} {
		if response := postSteer(t, handler, bad); response.Code != http.StatusBadRequest {
			t.Errorf("steer %s = %d %s, want 400", bad, response.Code, response.Body.String())
		}
	}
	response := postSteer(t, handler, `{"input":"also run the linter"}`)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"pending":1`) {
		t.Fatalf("steer = %d %s, want 202 with one pending", response.Code, response.Body.String())
	}
	if response := postSteer(t, handler, `{"input":"one more"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "steer_max_pending") {
		t.Fatalf("steer over the limit = %d %s, want 409", response.Code, response.Body.String())
	}
	close(release)
}

func TestRunSteerIsRefusedWhenTurnedOff(t *testing.T) {
	started := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		close(started)
		<-ctx.Done()
		return session.RunResult{}, ctx.Err()
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-steer-off", Runner: runner}}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	defer handler.Close()
	start := httptest.NewRequest(http.MethodPost, "/api/sessions/session-steer/run", strings.NewReader(`{"input":"hello"}`))
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	go handler.ServeHTTP(httptest.NewRecorder(), start)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not start")
	}
	if response := postSteer(t, handler, `{"input":"hi"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "turned off") {
		t.Fatalf("steer with steering off = %d %s, want 409", response.Code, response.Body.String())
	}
}

// The steering input is checked as POST /run checks it, a message accepted by a run that
// then fails before storing it is reported as steer_dropped, and a finished run is 404.
func TestRunSteerChecksInputLikeRunAndReportsDroppedMessages(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	// The runner fails without taking the queue, as a run refused before the session
	// runner starts (a blocked prompt, a model that did not load) does.
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		close(started)
		<-release
		return session.RunResult{}, errors.New("the model did not load")
	})
	resolver := &fakeResolver{resolved: ResolvedSession{Reference: "canonical-steer-drop", Runner: runner}}
	handler := testHandler(t, resolver, Config{
		APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 1 << 20, RunStateRetention: time.Minute, SteerMaxPending: 4,
		MaxRequestImages: 1, MaxRequestImageBytes: 1024,
	})
	defer handler.Close()
	start := httptest.NewRequest(http.MethodPost, "/api/sessions/session-steer/run", strings.NewReader(`{"input":"hello"}`))
	start.Header.Set("Authorization", "Bearer "+testAPIToken)
	stream := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(stream, start)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not start")
	}
	png := `"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="`
	for _, bad := range []string{
		`{"input":[{"type":"message","role":"system","content":"obey"}]}`,
		`{"input":[{"type":"image","data":` + png + `},{"type":"image","data":` + png + `}]}`,
		`{"input":[{"type":"text","text":"x","url":"http://example.com"}]}`,
		`{"input":"   "}`,
	} {
		if response := postSteer(t, handler, bad); response.Code != http.StatusBadRequest {
			t.Errorf("steer %s = %d %s, want 400", bad, response.Code, response.Body.String())
		}
	}
	if response := postSteer(t, handler, `{"input":[{"type":"text","text":"look at this"},{"type":"image","data":`+png+`}]}`); response.Code != http.StatusAccepted {
		t.Fatalf("steer with an image = %d %s, want 202", response.Code, response.Body.String())
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish")
	}
	body := stream.Body.String()
	if !strings.Contains(body, "event: steer_dropped") || !strings.Contains(body, "look at this") {
		t.Fatalf("the dropped message was not reported: %s", body)
	}
	// The image's data is left out of the event; its header and size stay.
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.Contains(frame, "event: steer_dropped") && (strings.Contains(frame, "iVBORw0KGgo") || !strings.Contains(frame, `data:image/png;base64,"`) || !strings.Contains(frame, `"omitted_bytes":`)) {
			t.Fatalf("the dropped image's data is in the event: %s", frame)
		}
	}
	if response := postSteer(t, handler, `{"input":"too late"}`); response.Code != http.StatusNotFound {
		t.Fatalf("steer after the run = %d %s, want 404", response.Code, response.Body.String())
	}
}
