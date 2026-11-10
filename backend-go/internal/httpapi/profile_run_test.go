package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/transcript"
)

func profileRunRequest(handler *Handler, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+testAPIToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type profileRunFixture struct {
	handler  *Handler
	mutation *fakeMutation
	resolver *transcriptLockedResolver
	started  chan struct{}
	release  chan struct{}
	inputs   chan string
}

// newProfileRunFixture has a runner that blocks until release is closed or its context
// ends, and commits one response before it returns.
func newProfileRunFixture(t *testing.T, maxRuns int) *profileRunFixture {
	t.Helper()
	f := &profileRunFixture{started: make(chan struct{}, 4), release: make(chan struct{}), inputs: make(chan string, 4)}
	runner := runnerFunc(func(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
		f.inputs <- string(input)
		f.started <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return session.RunResult{}, ctx.Err()
		}
		event := json.RawMessage(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`)
		if options.OnResponseCommitted != nil {
			options.OnResponseCommitted(event, transcript.CommitResult{})
		}
		return session.RunResult{TurnID: options.TurnID, Event: event}, nil
	})
	f.mutation = &fakeMutation{}
	f.resolver = &transcriptLockedResolver{
		fakeMaintenanceResolver: &fakeMaintenanceResolver{
			fakeResolver: &fakeResolver{resolved: ResolvedSession{Reference: "canonical", Profile: "dev", Runner: runner}},
			mutation:     f.mutation,
		},
		transcript: make(chan struct{}, 1),
		waiting:    make(chan struct{}, 1),
	}
	f.handler = testHandler(t, f.resolver, Config{APIToken: testAPIToken, MaxBodyBytes: 4096, SSEMaxBufferedBytes: 4096, RunStateRetention: time.Minute, MaxConcurrentRuns: maxRuns})
	t.Cleanup(f.handler.Close)
	return f
}

func (f *profileRunFixture) start(t *testing.T, ctx context.Context) *ProfileRun {
	t.Helper()
	run, err := f.handler.ProfileRuns().StartProfileRun(ctx, ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "caller-1", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func awaitProfileRunDone(t *testing.T, run *ProfileRun) {
	t.Helper()
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the profile run did not end")
	}
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
}

func TestStartProfileRunRunsTheTargetLikeAUserMessage(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	var committed atomic.Int32
	run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{
		Reference: "canonical", Profile: "dev", Caller: "caller-1", Message: "hello",
		OnCommitted: func(json.RawMessage) { committed.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	if got := <-f.inputs; got != `"hello"` {
		t.Fatalf("runner input = %s, want the message as a JSON string", got)
	}
	listed := f.handler.runs.SessionRuns("canonical", "").ByProfile["dev"]
	if listed.Source != "profile_message" || listed.Caller != "caller-1" || listed.Status != "running" {
		t.Fatalf("listed run = %+v", listed)
	}
	if run.TurnID() == "" {
		t.Fatal("the started run has no turn id")
	}
	if caller, ok := f.handler.ProfileRuns().RunningProfileRunCaller("canonical", "dev"); !ok || caller != "caller-1" {
		t.Fatalf("RunningProfileRunCaller = %q, %v", caller, ok)
	}
	close(f.release)
	awaitProfileRunDone(t, run)
	if run.Status() != "completed" || !strings.Contains(string(run.Final()), "answer") || committed.Load() != 1 {
		t.Fatalf("status %q, final %s, committed %d", run.Status(), run.Final(), committed.Load())
	}
	if _, ok := f.handler.ProfileRuns().RunningProfileRunCaller("canonical", "dev"); ok {
		t.Fatal("a finished run is still reported as running")
	}
}

func TestStartProfileRunReleasesTheTranscriptWhenTheRunEnds(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	run := f.start(t, context.Background())
	<-f.started
	if _, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "again"}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second start = %v, want ErrSessionBusy", err)
	}
	close(f.release)
	awaitProfileRunDone(t, run)
	deadline := time.Now().Add(5 * time.Second)
	for {
		release, err := f.resolver.TryLockSession(context.Background(), "canonical", "dev", "")
		if err == nil {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transcript stayed locked after the run ended: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartProfileRunIsNotCountedTowardTheCap(t *testing.T) {
	f := newProfileRunFixture(t, 1)
	if _, err := f.handler.beginRun("user\x00", "t1"); err != nil {
		t.Fatal(err)
	}
	f.start(t, context.Background())
	<-f.started
	if got := f.handler.runs.running.Load(); got != 1 {
		t.Fatalf("running = %d with one user run and one profile run, want 1", got)
	}
	close(f.release)
}

func TestStartProfileRunYieldsToAMutationAndBlocksOne(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	run := f.start(t, context.Background())
	<-f.started
	close(f.release)
	awaitProfileRunDone(t, run)

	if !f.handler.beginMutation("canonical\x00dev") {
		t.Fatal("mutation gate refused with no run")
	}
	_, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "x"})
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("start during a mutation = %v, want ErrSessionBusy", err)
	}
	f.handler.endMutation("canonical\x00dev")
}

func TestRunStreamShowsAProfileRun(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	run := f.start(t, context.Background())
	<-f.started
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(f.release)
	}()
	response := profileRunRequest(f.handler, http.MethodGet, "/api/sessions/session-1/run/stream?profile=dev")
	names := eventNames(parseEvents(t, response.Body.String()))
	if len(names) == 0 || names[0] != "run" || names[len(names)-1] != "done" {
		t.Fatalf("events = %v, want a run summary first and done last", names)
	}
	awaitProfileRunDone(t, run)
}

func TestRunStopStopsAProfileRunAndFreesTheProfileForAUser(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	run := f.start(t, context.Background())
	<-f.started
	response := profileRunRequest(f.handler, http.MethodPost, "/api/sessions/session-1/run/stop?profile=dev")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"stopped":true`) {
		t.Fatalf("stop status = %d body=%s", response.Code, response.Body.String())
	}
	awaitProfileRunDone(t, run)
	if run.Status() != "aborted" {
		t.Fatalf("status = %q, want aborted", run.Status())
	}
	// A user's message now starts at once: the transcript lock and the registry are free.
	started, busy, err := f.handler.tryStartRun(context.Background(), "canonical", "dev", "", "", "")
	deadline := time.Now().Add(5 * time.Second)
	for busy && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		started, busy, err = f.handler.tryStartRun(context.Background(), "canonical", "dev", "", "", "")
	}
	if err != nil || busy {
		t.Fatalf("a user start after the stop: busy=%v err=%v", busy, err)
	}
	started.run.finish("completed", nil)
	started.processUnlock()
}

func TestProfileRunStopFromTheCallerCancelsTheRun(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	run := f.start(t, context.Background())
	<-f.started
	run.Stop()
	awaitProfileRunDone(t, run)
	if run.Status() != "aborted" {
		t.Fatalf("status = %q, want aborted", run.Status())
	}
}

func TestStartProfileRunWithBusyWaitQueuesBehindTheHolderAndStarts(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	first := f.start(t, context.Background())
	<-f.started
	type result struct {
		run *ProfileRun
		err error
	}
	second := make(chan result, 1)
	go func() {
		run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c2", Message: "queued", Wait: true, WaitLimit: 5 * time.Second})
		second <- result{run, err}
	}()
	select {
	case <-f.resolver.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued start never waited for the transcript")
	}
	close(f.release)
	awaitProfileRunDone(t, first)
	select {
	case got := <-second:
		if got.err != nil {
			t.Fatalf("queued start: %v", got.err)
		}
		awaitProfileRunDone(t, got.run)
	case <-time.After(5 * time.Second):
		t.Fatal("the queued start did not run after the holder ended")
	}
}

func TestStartProfileRunWithBusyWaitReportsBusyWhenTheWaitEnds(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	f.start(t, context.Background())
	<-f.started
	began := time.Now()
	_, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c2", Message: "queued", Wait: true, WaitLimit: 100 * time.Millisecond})
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("error = %v, want ErrSessionBusy", err)
	}
	if elapsed := time.Since(began); elapsed < 90*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("waited %v, want about 100ms", elapsed)
	}
	if got := f.handler.startTurns.len(f.handler.turnKey("canonical", "dev")); got != 0 {
		t.Fatalf("turn queue length = %d after the wait ended, want 0", got)
	}
	close(f.release)
}

func TestStartProfileRunWithBusyWaitStopsWaitingWhenTheCallerEnds(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	f.start(t, context.Background())
	<-f.started
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := f.handler.ProfileRuns().StartProfileRun(ctx, ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c2", Message: "queued", Wait: true})
		errs <- err
	}()
	select {
	case <-f.resolver.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued start never waited")
	}
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionBusy) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued start did not end when the caller ended")
	}
	close(f.release)
}

func TestProfileRunFailureIsTheClientSafeMessage(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	f.resolver.resolved.Runner = runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		return session.RunResult{}, errors.New("secret /path detail")
	})
	run := f.start(t, context.Background())
	awaitProfileRunDone(t, run)
	if run.Status() != "failed" {
		t.Fatalf("status = %s, want failed", run.Status())
	}
	failure := run.Failure()
	if !strings.Contains(failure, "The run failed") || strings.Contains(failure, "secret") {
		t.Fatalf("failure = %q", failure)
	}
}

func TestStartProfileRunPanicBeforeLaunchFailsTheRunAndReleasesTheTranscript(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the BeforeRun panic did not propagate")
			}
		}()
		_, _ = f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{
			Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello",
			BeforeRun: func(context.Context) error { panic("before run failed") },
		})
	}()
	if _, ok := f.handler.ProfileRuns().RunningProfileRunCaller("canonical", "dev"); ok {
		t.Fatal("the run is still running")
	}
	if status := f.handler.runs.SessionRuns("canonical", "").ByProfile["dev"].Status; status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
	release, err := f.resolver.TryLockSession(context.Background(), "canonical", "dev", "")
	if err != nil {
		t.Fatalf("the transcript stayed locked: %v", err)
	}
	release()
}

// onEndRecorder records each OnEnd call. Its hook calls Status and checks the transcript lock
// is free, so a hook that runs under that lock, or that deadlocks on the run's mutex, fails.
type onEndRecorder struct {
	t     *testing.T
	f     *profileRunFixture
	calls chan string
}

func newOnEndRecorder(t *testing.T, f *profileRunFixture) *onEndRecorder {
	return &onEndRecorder{t: t, f: f, calls: make(chan string, 8)}
}

func (r *onEndRecorder) hook(run *ProfileRun) {
	status := run.Status()
	release, err := r.f.resolver.TryLockSession(context.Background(), "canonical", "dev", "")
	if err != nil {
		r.t.Errorf("the transcript was held inside OnEnd: %v", err)
	} else {
		release()
	}
	r.calls <- status
}

// awaitOnEnd waits for one hook call and fails if a second call arrives shortly after.
func (r *onEndRecorder) awaitOnEnd(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.calls:
		if got != want {
			t.Fatalf("OnEnd saw status %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnEnd did not run")
	}
	select {
	case got := <-r.calls:
		t.Fatalf("OnEnd ran twice; second status %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProfileRunOnEndFiresOnceAfterCompletion(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	recorder := newOnEndRecorder(t, f)
	run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello", OnEnd: recorder.hook})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	close(f.release)
	awaitProfileRunDone(t, run)
	recorder.awaitOnEnd(t, "completed")
}

func TestProfileRunOnEndFiresOnceAfterFailure(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	recorder := newOnEndRecorder(t, f)
	failing := runnerFunc(func(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
		return session.RunResult{}, errors.New("provider down")
	})
	f.resolver.fakeResolver.resolved.Runner = failing
	run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello", OnEnd: recorder.hook})
	if err != nil {
		t.Fatal(err)
	}
	awaitProfileRunDone(t, run)
	recorder.awaitOnEnd(t, "failed")
}

func TestProfileRunOnEndFiresOnceAfterAbort(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	recorder := newOnEndRecorder(t, f)
	run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello", OnEnd: recorder.hook})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	run.Stop()
	awaitProfileRunDone(t, run)
	recorder.awaitOnEnd(t, "aborted")
}

func TestProfileRunOnEndFiresOnceWhenTheStartFails(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	recorder := newOnEndRecorder(t, f)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the BeforeRun panic did not propagate")
			}
		}()
		_, _ = f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{
			Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello", OnEnd: recorder.hook,
			BeforeRun: func(context.Context) error { panic("before run failed") },
		})
	}()
	recorder.awaitOnEnd(t, "failed")
}

// A BeforeRun that fails ends the start: the error comes back, the run is failed and gone from
// the registry's running set, the transcript is released, OnEnd fires once, and the target run
// itself never began.
func TestStartProfileRunBeforeRunErrorFailsTheStartWithoutRunning(t *testing.T) {
	f := newProfileRunFixture(t, 0)
	recorder := newOnEndRecorder(t, f)
	want := errors.New("before run refused")
	run, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{
		Reference: "canonical", Profile: "dev", Caller: "c", Message: "hello", OnEnd: recorder.hook,
		BeforeRun: func(context.Context) error { return want },
	})
	if !errors.Is(err, want) || run != nil {
		t.Fatalf("StartProfileRun = %v, %v; want the BeforeRun error and no run", run, err)
	}
	if _, ok := f.handler.ProfileRuns().RunningProfileRunCaller("canonical", "dev"); ok {
		t.Fatal("the run is still running")
	}
	if status := f.handler.runs.SessionRuns("canonical", "").ByProfile["dev"].Status; status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
	recorder.awaitOnEnd(t, "failed")
	select {
	case <-f.started:
		t.Fatal("the target run began although BeforeRun failed")
	default:
	}
	// A message right after finds the profile free.
	next, err := f.handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{Reference: "canonical", Profile: "dev", Caller: "c", Message: "again"})
	if err != nil {
		t.Fatalf("a start after the failed one: %v", err)
	}
	if next != nil {
		next.Stop()
		awaitProfileRunDone(t, next)
	}
}
