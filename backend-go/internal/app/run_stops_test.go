package app

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools/native"
)

// R5c follow-up: a user stop cancels the run and then stops the owner's work; a call of the run
// still in flight can start work after that fan-out looked. Such work is entered here
// deterministically: once the stopped run's runner returned (runnerReturned) and before the run
// ends, the test registers a handle under the run's lineage, as that call would, owned elsewhere
// so that only the run's own stop cascade can reach it.

// lateDriver is an async driver that records its stops and ends each handle at once.
type lateDriver struct {
	m     *asyncmgr.Manager
	mu    sync.Mutex
	stops []string
}

func (d *lateDriver) Kind() asyncmgr.Kind { return "late_work" }

func (d *lateDriver) Stop(id, reason string) {
	d.mu.Lock()
	d.stops = append(d.stops, id+":"+reason)
	d.mu.Unlock()
	_, _ = d.m.Complete(id, asyncmgr.Result{State: asyncmgr.StateStopped, Reason: reason})
}

func (d *lateDriver) stopped() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.stops)
}

// lateRuns runs dev/coder and, between each run's runner return and its end, hands the run's
// context to during.
type lateRuns struct {
	f      *layoutFixture
	driver *lateDriver
	during func(ctx context.Context)
}

func newLateRuns(t *testing.T) *lateRuns {
	t.Helper()
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", ""))
	l := &lateRuns{f: f}
	f.service.resolver.runnerReturned = func(ctx context.Context) {
		if l.driver == nil {
			managers := f.service.resolver.asyncManagers()
			if len(managers) == 0 {
				t.Error("dev/coder has no async manager")
				return
			}
			l.driver = &lateDriver{m: managers[0]}
			if err := managers[0].AddDriver(l.driver); err != nil {
				t.Error(err)
				return
			}
		}
		if l.during != nil {
			l.during(ctx)
		}
	}
	return l
}

// register starts handle id under the lineage of ctx, owned by nobody a fan-out names.
func (l *lateRuns) register(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	err := l.driver.m.Register(asyncmgr.Registration{ID: id, Owner: "elsewhere", Kind: "late_work", Label: id, Lineage: native.RunLineageOf(ctx)})
	if err != nil {
		t.Error(err)
	}
}

func (l *lateRuns) run(t *testing.T) {
	t.Helper()
	status, body := l.f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("dev/coder"), `{"input":"hello"}`)
	if status != http.StatusOK || !strings.Contains(body, "event: completed") && !strings.Contains(body, "event: stopped") && !strings.Contains(body, "event: error") {
		t.Fatalf("run: %d %s", status, body)
	}
}

func (l *lateRuns) stop(t *testing.T) {
	t.Helper()
	if status, body := l.f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("dev/coder"), ""); status != http.StatusOK || !strings.Contains(body, `"Stopped":true`) && !strings.Contains(body, `"stopped":true`) {
		t.Fatalf("stop: %d %s", status, body)
	}
}

func (l *lateRuns) state(t *testing.T, id string) asyncmgr.State {
	t.Helper()
	entry, err := l.driver.m.Status("elsewhere", id)
	if err != nil {
		t.Fatal(err)
	}
	return entry.State
}

// The reviewer's case for the user's own run: work its in-flight call registers after the user
// stop's fan-out is stopped once the run ends, with the stop's reason.
func TestUserStopReachesWorkTheStoppedRunStartedAfterTheFanOut(t *testing.T) {
	l := newLateRuns(t)
	l.during = func(ctx context.Context) {
		l.stop(t) // cancels the run and runs the owner fan-out, which finds nothing yet
		l.register(t, ctx, "late_1")
	}
	l.run(t)
	if got := l.driver.stopped(); !slices.Equal(got, []string{"late_1:user_stop"}) {
		t.Fatalf("stops = %q, want the late work stopped once with user_stop", got)
	}
}

// Only the stopped run's own work is reached: an earlier run's and a later run's work under their
// own keys go on, and a fan-out that reached a run nothing cancelled (no stop of it) cascades
// nothing.
func TestUserStopLeavesTheWorkOfEarlierAndLaterRunsAlone(t *testing.T) {
	l := newLateRuns(t)
	steps := []func(ctx context.Context){
		func(ctx context.Context) { l.register(t, ctx, "earlier") },
		func(ctx context.Context) {
			l.stop(t)
			l.register(t, ctx, "stopped")
		},
		func(ctx context.Context) { l.register(t, ctx, "later") },
		func(ctx context.Context) {
			// The owner fan-out notes this run, but nothing cancels it: it ends on its own.
			l.f.service.resolver.CancelSessionChildren(sessionReference(t, l.f), "dev/coder")
			l.register(t, ctx, "noted")
		},
	}
	for _, step := range steps {
		l.during = step
		l.run(t)
	}
	if got := l.driver.stopped(); !slices.Equal(got, []string{"stopped:user_stop"}) {
		t.Fatalf("stops = %q, want only the stopped run's work", got)
	}
	for _, id := range []string{"earlier", "later", "noted"} {
		if state := l.state(t, id); state != asyncmgr.StateRunning {
			t.Errorf("%s is %s, want running", id, state)
		}
	}
	stops := &l.f.service.resolver.runStops
	stops.mu.Lock()
	n := len(stops.runs)
	stops.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d runs still recorded after every run ended", n)
	}
}

func sessionReference(t *testing.T, f *layoutFixture) string {
	t.Helper()
	reference, err := f.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

// A session delete notes every profile's run before it stops them (stopSessionRuns), so the work a
// run registers after the delete's fan-out stops with the delete's reason.
func TestSessionDeleteReachesWorkTheStoppedRunStartedAfterTheFanOut(t *testing.T) {
	l := newLateRuns(t)
	l.during = func(ctx context.Context) {
		l.f.service.resolver.CancelAllSessionChildren(sessionReference(t, l.f))
		l.stop(t) // the run stop that follows the delete's fan-out; its own note comes second
		l.register(t, ctx, "late_1")
	}
	l.run(t)
	if got := l.driver.stopped(); !slices.Equal(got, []string{"late_1:session_delete"}) {
		t.Fatalf("stops = %q, want the late work stopped once with session_delete", got)
	}
}

// registerAs starts handle id owned by owner under the lineage of ctx.
func (l *lateRuns) registerAs(t *testing.T, ctx context.Context, owner, id string) {
	t.Helper()
	err := l.driver.m.Register(asyncmgr.Registration{ID: id, Owner: owner, Kind: "late_work", Label: id, Lineage: native.RunLineageOf(ctx)})
	if err != nil {
		t.Error(err)
	}
}

// gone reports whether the async manager no longer holds handle id of owner.
func (l *lateRuns) gone(t *testing.T, owner, id string) bool {
	t.Helper()
	_, err := l.driver.m.Status(owner, id)
	return err != nil
}

// The work a run registers after a session delete's fan-out is deleted as the fan-out deletes the
// session's own work: the deleted session's handles are stopped and their records removed (a live
// one when it ends, a finished one at once), while a handle of another owner under the same
// lineage is only stopped and keeps its record, and work outside the run's lineage is untouched.
func TestSessionDeleteRemovesTheRecordsOfWorkTheStoppedRunStartedAfterTheFanOut(t *testing.T) {
	l := newLateRuns(t)
	reference := sessionReference(t, l.f)
	own := sessionKey(reference, "dev/coder")
	sibling := sessionKey(reference, "review/reviewer") // another profile of the deleted session
	l.during = func(ctx context.Context) {
		l.registerAs(t, context.Background(), "elsewhere", "unrelated")
		l.f.service.resolver.CancelAllSessionChildren(reference)
		l.stop(t)
		l.registerAs(t, ctx, own, "late_own")
		l.registerAs(t, ctx, own, "late_done")
		if _, err := l.driver.m.Complete("late_done", asyncmgr.Result{State: asyncmgr.StateDone, OK: true}); err != nil {
			t.Error(err)
		}
		l.registerAs(t, ctx, sibling, "late_sibling")
		l.registerAs(t, ctx, "elsewhere", "late_other")
	}
	l.run(t)
	got := l.driver.stopped()
	slices.Sort(got)
	if want := []string{"late_other:session_delete", "late_own:session_delete", "late_sibling:session_delete"}; !slices.Equal(got, want) {
		t.Fatalf("stops = %q, want %q", got, want)
	}
	for owner, id := range map[string]string{own: "late_own", sibling: "late_sibling"} {
		if !l.gone(t, owner, id) {
			t.Errorf("%s of the deleted session still has a record", id)
		}
	}
	if !l.gone(t, own, "late_done") {
		t.Error("late_done of the deleted session still has a record")
	}
	if state := l.state(t, "late_other"); state != asyncmgr.StateStopped {
		t.Errorf("late_other is %s, want stopped with its record kept", state)
	}
	if state := l.state(t, "unrelated"); state != asyncmgr.StateRunning {
		t.Errorf("unrelated is %s, want running", state)
	}
}

// A delete that comes after a user stop already noted the run still deletes the late work: the
// delete's end is the stronger one, as in the async manager (endRemove over endKeep).
func TestSessionDeleteAfterAUserStopStillRemovesTheLateWork(t *testing.T) {
	l := newLateRuns(t)
	reference := sessionReference(t, l.f)
	own := sessionKey(reference, "dev/coder")
	l.during = func(ctx context.Context) {
		l.stop(t)
		l.f.service.resolver.CancelAllSessionChildren(reference)
		l.registerAs(t, ctx, own, "late_own")
	}
	l.run(t)
	if got := l.driver.stopped(); !slices.Equal(got, []string{"late_own:session_delete"}) {
		t.Fatalf("stops = %q, want the late work stopped once with session_delete", got)
	}
	if !l.gone(t, own, "late_own") {
		t.Fatal("late_own of the deleted session still has a record")
	}
}
