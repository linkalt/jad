package httpapi

import (
	"context"
	"encoding/json"
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
