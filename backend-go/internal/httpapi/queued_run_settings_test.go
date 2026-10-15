package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

// transcriptLockedResolver behaves like the real resolver for run starts: the
// transcript lock is exclusive and held until the run ends, TryLockSession
// reports ErrSessionBusy at once, and a waiting LockSession gives up when its
// request is cancelled.
type transcriptLockedResolver struct {
	*fakeMaintenanceResolver
	transcript chan struct{}
	waiting    chan struct{}

	hookMu    sync.Mutex
	onRelease func()
}

func (r *transcriptLockedResolver) setOnRelease(hook func()) {
	r.hookMu.Lock()
	r.onRelease = hook
	r.hookMu.Unlock()
}

func (r *transcriptLockedResolver) releaseTranscript() {
	r.hookMu.Lock()
	hook := r.onRelease
	r.hookMu.Unlock()
	if hook != nil {
		hook()
	}
	<-r.transcript
}

func (r *transcriptLockedResolver) TryLockSession(context.Context, string, string, string) (func(), error) {
	select {
	case r.transcript <- struct{}{}:
		return r.releaseTranscript, nil
	default:
		return nil, ErrSessionBusy
	}
}

func (r *transcriptLockedResolver) LockSession(ctx context.Context, _, _, _ string) (func(), error) {
	select {
	case r.waiting <- struct{}{}:
	default:
	}
	select {
	case r.transcript <- struct{}{}:
		return r.releaseTranscript, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *transcriptLockedResolver) LockSessionSettings(context.Context, string) (func(), error) {
	return func() {}, nil
}

func (r *transcriptLockedResolver) UpdateModelDefault(context.Context, string, string, string) (SessionModelDefaultUpdate, error) {
	return SessionModelDefaultUpdate{ModelDefault: "models/new.json", Changed: true}, nil
}

func (r *transcriptLockedResolver) ListSessionFiles(context.Context, string, string) (SessionFileListing, error) {
	return SessionFileListing{}, nil
}

// queuedRunFixture has one run in flight and a second run start waiting for the
// transcript behind it.
type queuedRunFixture struct {
	t         *testing.T
	handler   *Handler
	mutation  *fakeMutation
	secondEnd context.CancelFunc
	second    <-chan *httptest.ResponseRecorder
	first     <-chan *httptest.ResponseRecorder
	release   chan struct{}
	resolver  *transcriptLockedResolver
}

func newQueuedRunFixture(t *testing.T) *queuedRunFixture {
	t.Helper()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ json.RawMessage, _ session.RunOptions) (session.RunResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return session.RunResult{}, nil
	})
	mutation := &fakeMutation{}
	resolver := &transcriptLockedResolver{
		fakeMaintenanceResolver: &fakeMaintenanceResolver{
			fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev", Runner: runner}},
			mutation:     mutation,
		},
		transcript: make(chan struct{}, 1),
		waiting:    make(chan struct{}, 1),
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	f := &queuedRunFixture{t: t, handler: handler, mutation: mutation, release: release, resolver: resolver}
	t.Cleanup(func() {
		f.endQueuedStart()
		select {
		case <-release:
		default:
			close(release)
		}
		handler.Close()
	})

	f.first = f.serve(context.Background(), "/api/sessions/s/run?profile=dev", `{"input":"one"}`)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}
	select {
	case <-resolver.waiting:
	default:
	}
	secondCtx, cancel := context.WithCancel(context.Background())
	f.secondEnd = cancel
	f.second = f.serve(secondCtx, "/api/sessions/s/run?profile=dev", `{"input":"two"}`)
	select {
	case <-resolver.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("second run start did not wait for the transcript")
	}
	return f
}

func (f *queuedRunFixture) endQueuedStart() {
	if f.secondEnd != nil {
		f.secondEnd()
	}
}

func (f *queuedRunFixture) serve(ctx context.Context, path, body string) <-chan *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		f.handler.ServeHTTP(response, request)
		done <- response
	}()
	return done
}

func (f *queuedRunFixture) answer(path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	select {
	case response := <-f.serve(context.Background(), path, body):
		return response
	case <-time.After(2 * time.Second):
		f.t.Fatalf("%s was blocked behind the queued run start", path)
		return nil
	}
}

// A second run start waits for the transcript outside the in-process session
// lock, so the other writers of that session are not blocked behind it.
func TestQueuedRunStartDoesNotBlockSessionSettings(t *testing.T) {
	f := newQueuedRunFixture(t)
	response := f.answer("/api/sessions/s/model-default?profile=dev", `{"model_default":"models/new.json"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("model-default status = %d body=%s", response.Code, response.Body.String())
	}
}

// The reset check relies on a registered run being visible whenever the
// transcript is taken: a reset sent while a start is queued is still refused.
func TestResetIsRefusedWhileARunIsActiveAndAStartIsQueued(t *testing.T) {
	f := newQueuedRunFixture(t)
	response := f.answer("/api/sessions/s/reset?profile=dev", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "cannot reset a profile while a run is active") {
		t.Fatalf("reset status = %d body=%s", response.Code, response.Body.String())
	}
	if f.mutation.reset {
		t.Fatal("reset ran during an active run")
	}
}

// When the first run ends, the queued start takes the transcript and runs.
func TestQueuedRunStartRunsAfterTheFirstRunEnds(t *testing.T) {
	f := newQueuedRunFixture(t)
	close(f.release)
	select {
	case response := <-f.second:
		if response.Code != http.StatusOK {
			t.Fatalf("queued start status = %d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued run start never ran after the first run ended")
	}
}

// A client that disconnects while its start is queued leaves nothing behind.
func TestQueuedRunStartEndsWhenItsClientDisconnects(t *testing.T) {
	f := newQueuedRunFixture(t)
	f.endQueuedStart()
	select {
	case response := <-f.second:
		if response.Code == http.StatusOK {
			t.Fatalf("cancelled start answered 200: %s", response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled run start is still waiting")
	}
	if held := len(f.resolver.transcript); held != 1 {
		t.Fatalf("transcript tokens = %d, want 1 (the first run's only)", held)
	}
	if runs := len(f.handler.runs.runs["canonical\x00dev"]); runs != 1 {
		t.Fatalf("registered runs = %d, want 1 (the cancelled start must not register)", runs)
	}
}

func newIdleTranscriptResolver(t *testing.T) (*transcriptLockedResolver, *Handler) {
	t.Helper()
	resolver := &transcriptLockedResolver{
		fakeMaintenanceResolver: &fakeMaintenanceResolver{
			fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev", Runner: runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
				return session.RunResult{}, nil
			})}},
			mutation: &fakeMutation{},
		},
		transcript: make(chan struct{}, 1),
		waiting:    make(chan struct{}, 1),
	}
	handler := testHandler(t, resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute})
	t.Cleanup(handler.Close)
	return resolver, handler
}

func postRun(handler *Handler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/s/run?profile=dev", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A start that fails after it took the transcript lock must give it back, or the
// session could never run again.
func TestRunStartReleasesTheTranscriptWhenResolveFails(t *testing.T) {
	resolver, handler := newIdleTranscriptResolver(t)
	resolver.err = &StatusError{Code: http.StatusBadRequest, Message: "bad model"}
	if response := postRun(handler); response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if held := len(resolver.transcript); held != 0 {
		t.Fatalf("transcript lock still held after a failed Resolve (%d)", held)
	}
}

func TestRunStartReleasesTheTranscriptWhenTheMutationGateRefuses(t *testing.T) {
	resolver, handler := newIdleTranscriptResolver(t)
	if !handler.beginMutation("canonical\x00dev") {
		t.Fatal("could not register a mutation")
	}
	defer handler.endMutation("canonical\x00dev")
	if response := postRun(handler); response.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if held := len(resolver.transcript); held != 0 {
		t.Fatalf("transcript lock still held after the gate refused the run (%d)", held)
	}
}
