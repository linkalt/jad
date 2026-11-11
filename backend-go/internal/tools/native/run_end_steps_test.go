package native

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A panic inside one cleanup step of a run's end (endRun), not before it (T-007 review): the later
// steps still run, the step that panicked is not retried, the panic is logged with the step and the
// run, the steps that depend on the failed one carry on with what is left, and calling endRun again
// repeats nothing.

// endStepsFixture is a background writer catalog that keeps one finished run, records each
// manifest removal, each dropped identity and each run-end notification, and logs to logged.
type endStepsFixture struct {
	manager   *SubagentManager
	catalog   func(arguments string) string
	handoff   func(t *testing.T) (string, bool)
	release   chan struct{}
	logged    *lockedLog
	mu        sync.Mutex
	removed   []string
	dropped   int
	notified  atomic.Int64
	asyncWait func() string
}

func newEndStepsFixture(t *testing.T) *endStepsFixture {
	t.Helper()
	fixture := &endStepsFixture{release: make(chan struct{}), logged: &lockedLog{}}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(fixture.logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var calls atomic.Int64
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		if calls.Add(1) == 1 {
			return SubagentResult{Text: "first"}, nil
		}
		select {
		case <-fixture.release:
			return SubagentResult{Text: "fixed"}, nil
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
	}}
	settings := backgroundWriterSettings(1, writerAgent("fixer", "read", "exec"))
	settings.KeepFinished = 1
	catalog, manager, host := backgroundCatalog(t, settings, launcher, readOnlyTool("read"), writeTool("exec"))
	t.Cleanup(host.Close)
	fixture.manager = manager
	manager.removeManifest = func(id string) {
		fixture.mu.Lock()
		fixture.removed = append(fixture.removed, id)
		fixture.mu.Unlock()
	}
	under := WithRunLineage(context.Background(), []string{"demo\x00dev/coder"})
	fixture.catalog = func(arguments string) string { return subagentCallWith(t, under, catalog, arguments) }
	fixture.asyncWait = func() string { return asyncWait(t, catalog) }
	fixture.handoff = func(t *testing.T) (string, bool) {
		done := make(chan string, 1)
		go func() { done <- subagentCallWith(t, under, catalog, `{"task":"two","block_ms":0}`) }()
		select {
		case out := <-done:
			return out, true
		case <-time.After(2 * time.Second):
			return "", false
		}
	}
	return fixture
}

func (f *endStepsFixture) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// runEnded runs the first run to its end, then hands off the second and releases it, and returns
// both ids once the second's handle is terminal.
func (f *endStepsFixture) runBoth(t *testing.T) (first, second string) {
	t.Helper()
	if out := f.catalog(`{"task":"one"}`); !strings.Contains(out, "finished") {
		t.Fatalf("the first run: %s", out)
	}
	f.manager.mu.Lock()
	if len(f.manager.finished) != 1 {
		f.manager.mu.Unlock()
		t.Fatalf("finished list after the first run: %v", f.manager.finished)
	}
	first = f.manager.finished[0]
	f.manager.mu.Unlock()
	out, returned := f.handoff(t)
	if !returned {
		close(f.release)
		t.Fatal("the second run was not handed off")
	}
	second = runIDOf(t, out)
	close(f.release)
	got := f.asyncWait()
	for _, want := range []string{second, "done", "fixed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the handle of the run whose end step panicked is not terminal with its answer (lacks %q):\n%s", want, got)
		}
	}
	return first, second
}

// requireEndedWhole checks every resource of the second run is given back and the run is listed.
func (f *endStepsFixture) requireEndedWhole(t *testing.T, second string) *subagentRun {
	t.Helper()
	manager := f.manager
	eventually(t, "the writer place to be given back", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
	awaitNoLiveTreeRuns(t, manager, "a panic inside an end step")
	eventually(t, "the slot to be given back", func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.running == 0
	})
	manager.mu.Lock()
	run := manager.runs[second]
	finished := append([]string(nil), manager.finished...)
	manager.mu.Unlock()
	if run == nil {
		t.Fatalf("the second run %s was forgotten", second)
	}
	select {
	case <-run.done:
	default:
		t.Fatal("the run's waiters were never woken")
	}
	if len(finished) != 1 || finished[0] != second {
		t.Fatalf("finished list %v, want only %s (keep_finished 1)", finished, second)
	}
	return run
}

// requireRepeatSafe calls endRun again and checks it repeats no step.
func (f *endStepsFixture) requireRepeatSafe(t *testing.T, run *subagentRun, dropped int) {
	t.Helper()
	removed, notified := len(f.removedIDs()), f.notified.Load()
	tree := f.manager.runTree()
	tree.ReserveWriter(run.owner, 0, false) // a second writer of the owner, which a repeated release would take
	f.manager.endRun(run, subagentStart{keep: 1}, "fixed")
	writers := tree.Writers(run.owner)
	tree.ReleaseWriter(run.owner)
	f.manager.mu.Lock()
	finished := len(f.manager.finished)
	f.manager.mu.Unlock()
	if writers != 1 || finished != 1 {
		t.Fatalf("a repeated endRun gave back the writer place again (%d left) or listed the run again (%d listed)", writers, finished)
	}
	f.mu.Lock()
	gotDropped := f.dropped
	f.mu.Unlock()
	if got := len(f.removedIDs()); got != removed || gotDropped != dropped || f.notified.Load() != notified {
		t.Fatalf("a repeated endRun repeated a step: removed %d→%d, dropped %d, notified %d→%d", removed, got, gotDropped, notified, f.notified.Load())
	}
}

// The trim step drops the first run: the dropped callback panics for it. The removal of its manifest
// (which depends on the callback's step) still runs, the writer place, the handle, done and the
// notification still run, the callback is called once, and the panic is logged with the step.
func TestAPanicInsideTheDropStepOfARunsEndStillRunsTheLaterSteps(t *testing.T) {
	fixture := newEndStepsFixture(t)
	fixture.manager.SetOnDropped(func(any) {
		fixture.mu.Lock()
		fixture.dropped++
		fixture.mu.Unlock()
		panic("dropped exploded")
	})
	fixture.manager.SetRunEnd(func() { fixture.notified.Add(1) })
	first, second := fixture.runBoth(t)
	run := fixture.requireEndedWhole(t, second)
	fixture.mu.Lock()
	dropped := fixture.dropped
	fixture.mu.Unlock()
	if dropped != 1 {
		t.Fatalf("the dropped callback ran %d times, want once", dropped)
	}
	if removed := fixture.removedIDs(); len(removed) != 1 || removed[0] != first {
		t.Fatalf("manifests removed %v, want the dropped run %s's: the removal after the panicking callback was skipped", removed, first)
	}
	if got := fixture.notified.Load(); got != 2 {
		t.Fatalf("the run-end notification ran %d times, want once per run", got)
	}
	// The dropped run is the one its removal step is about.
	for _, want := range []string{"dropped exploded", "step=dropped_callback", "run=" + first} {
		if log := fixture.logged.String(); !strings.Contains(log, want) {
			t.Fatalf("the panic is not logged with %q:\n%s", want, log)
		}
	}
	fixture.requireRepeatSafe(t, run, 1)
}

// The notification, the last step, panics once: it is not tried again (neither by the recovery nor
// by a repeated endRun), and everything before it was given back.
func TestAPanicInsideTheLastStepOfARunsEndIsNotRetried(t *testing.T) {
	fixture := newEndStepsFixture(t)
	fixture.manager.SetRunEnd(func() {
		if fixture.notified.Add(1) == 2 {
			panic("notify exploded")
		}
	})
	_, second := fixture.runBoth(t)
	run := fixture.requireEndedWhole(t, second)
	eventually(t, "the second run's notification", func() bool { return fixture.notified.Load() >= 2 })
	if got := fixture.notified.Load(); got != 2 {
		t.Fatalf("the notifications ran %d times, want twice (once per run): the panicking one was retried", got)
	}
	if log := fixture.logged.String(); !strings.Contains(log, "notify exploded") || !strings.Contains(log, "run="+second+" step=run_end_notify") {
		t.Fatalf("the panic is not logged with the run:\n%s", log)
	}
	fixture.requireRepeatSafe(t, run, 0)
}
