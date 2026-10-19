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

func newProfileRunHandler(t *testing.T, maxRuns int) (*Handler, *fakeMutation) {
	t.Helper()
	mutation := &fakeMutation{}
	resolver := &fakeMaintenanceResolver{
		fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev", Runner: runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
			return session.RunResult{}, nil
		})}},
		mutation: mutation,
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxConcurrentRuns: maxRuns})
	t.Cleanup(handler.Close)
	return handler, mutation
}

func profileRunRequest(handler *Handler, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSessionRunsCarriesSourceAndCallerOnlyForProfileRuns(t *testing.T) {
	registry := newRunRegistry(time.Minute, 4096, 0)
	if _, err := registry.begin("alpha\x00user", "turn-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.beginFrom("alpha\x00green", "turn-nested", "profile_message", "caller-1"); err != nil {
		t.Fatal(err)
	}
	summaries := registry.SessionRuns("alpha", "")
	green := summaries.ByProfile["green"]
	if green.Source != "profile_message" || green.Caller != "caller-1" || green.Status != "running" {
		t.Fatalf("green run = %+v, want source and caller", green)
	}
	user, err := json.Marshal(summaries.ByProfile["user"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(user), "source") || strings.Contains(string(user), "caller") {
		t.Fatalf("user run JSON = %s, want no source or caller", user)
	}
	want := `{"turn_id":"turn-user","started_at":"` + summaries.ByProfile["user"].StartedAt + `","status":"running"}`
	if string(user) != want {
		t.Fatalf("user run JSON = %s, want %s", user, want)
	}
}

func TestProfileRunIsNotCountedTowardTheCap(t *testing.T) {
	handler, _ := newProfileRunHandler(t, 1)
	if _, err := handler.beginRun("user\x00", "t1"); err != nil {
		t.Fatal(err)
	}
	_, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatalf("nested run refused with the cap full: %v", err)
	}
	if got := handler.runs.running.Load(); got != 1 {
		t.Fatalf("running = %d with one user run and one nested run, want 1", got)
	}
	_, err = handler.beginRun("other\x00", "t2")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusTooManyRequests {
		t.Fatalf("user run at cap 1 = %#v, want 429", err)
	}
	finish("completed")
	finish("failed")
	if got := handler.runs.running.Load(); got != 1 {
		t.Fatalf("running = %d after the nested run finished, want 1", got)
	}
}

func TestProfileRunDoesNotConsumeASlot(t *testing.T) {
	handler, _ := newProfileRunHandler(t, 1)
	_, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	defer finish("completed")
	if _, err := handler.beginRun("other\x00", "t2"); err != nil {
		t.Fatalf("user run refused beside a nested run at cap 1: %v", err)
	}
}

func TestProfileRunIsListedWhileRunningAndRetiredAtOnce(t *testing.T) {
	handler, _ := newProfileRunHandler(t, 0)
	_, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	listed := handler.runs.SessionRuns("canonical", "").ByProfile["dev"]
	if listed.Source != "profile_message" || listed.Caller != "caller-1" || listed.TurnID != "turn-1" {
		t.Fatalf("listed run = %+v", listed)
	}
	finish("completed")
	if run := handler.runs.current("canonical\x00dev"); run != nil {
		t.Fatalf("nested run still retained with status %q", run.state())
	}
	if got := handler.runs.SessionRuns("canonical", "").ByProfile; len(got) != 0 {
		t.Fatalf("runs by profile after finish = %+v, want none", got)
	}
}

func TestProfileRunBlocksMutationsAndYieldsToThem(t *testing.T) {
	handler, mutation := newProfileRunHandler(t, 0)
	_, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	response := profileRunRequest(handler, http.MethodPost, "/api/sessions/session-1/reset?profile=dev")
	if response.Code != http.StatusConflict || mutation.reset {
		t.Fatalf("reset during nested run: status=%d reset=%v body=%s", response.Code, mutation.reset, response.Body.String())
	}
	finish("completed")

	if !handler.beginMutation("canonical\x00dev") {
		t.Fatal("mutation gate refused with no run")
	}
	_, _, err = handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-2", "caller-1")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusConflict {
		t.Fatalf("nested begin during a mutation = %#v, want 409", err)
	}
	handler.endMutation("canonical\x00dev")
}

func TestRunStreamIsIdleForAProfileRun(t *testing.T) {
	handler, _ := newProfileRunHandler(t, 0)
	_, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	defer finish("completed")
	response := profileRunRequest(handler, http.MethodGet, "/api/sessions/session-1/run/stream?profile=dev")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got, want := eventNames(parseEvents(t, response.Body.String())), []string{"idle"}; !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRunStopCancelsAProfileRunContext(t *testing.T) {
	handler, _ := newProfileRunHandler(t, 0)
	runCtx, finish, err := handler.ProfileRuns().BeginProfileRun("canonical", "dev", "turn-1", "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	defer finish("aborted")
	if runCtx.Err() != nil {
		t.Fatal("run context cancelled before stop")
	}
	response := profileRunRequest(handler, http.MethodPost, "/api/sessions/session-1/run/stop?profile=dev")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"stopped":true`) {
		t.Fatalf("stop status = %d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the nested run context")
	}
}
