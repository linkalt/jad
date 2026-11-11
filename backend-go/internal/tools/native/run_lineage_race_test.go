package native

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// R5c review: a stop is chosen while a run is live and applied after, outside the chooser's lock.
// The run can start work in that gap and end done or failed before the stop lands; that work must
// still stop. These tests put the stop in each gap deterministically.

// cascadeLog records what a manager hands to its stop cascade.
type cascadeLog struct {
	mu   sync.Mutex
	told []string
}

func (l *cascadeLog) note(key, reason string) {
	l.mu.Lock()
	l.told = append(l.told, key+" "+reason)
	l.mu.Unlock()
}

func (l *cascadeLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.told)
}

// A stop that lands after the launcher returned an answer leaves the run done, but the work it
// started carries its lineage key and is reached: the key goes to the cascade with that stop's reason.
func TestAStopThatLandsAfterTheLauncherReturnedStillCascades(t *testing.T) {
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	log := &cascadeLog{}
	manager.SetStopCascade(log.note)
	manager.afterLaunch = func() { manager.CancelUnmanagedLineage(keyIs("k1"), "caller_stopped") }
	parent := withExecutors(t, readOnlyTool("read"))
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "answer"}, nil
	}}
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCallWith(t, WithRunLineage(context.Background(), []string{"k1"}), catalog, `{"task":"t"}`)
	runs := manager.allRuns()
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	id, key := runs[0].id, runs[0].lineageKey
	if view := manager.byID(id).view(); view.state != subagentRunDone {
		t.Fatalf("run = %+v, want done: the answer came before the stop", view)
	}
	if got := log.list(); !slices.Equal(got, []string{key + " caller_stopped"}) {
		t.Fatalf("cascade told %q, want only %s with the stop's reason", got, key)
	}
	// A run that has ended is chosen by no later stop, so its work is not reached again.
	if n := manager.CancelUnmanagedLineage(keyIs("k1"), "user_stop"); n != 0 || len(log.list()) != 1 {
		t.Fatalf("a later stop chose %d ended runs and told %q", n, log.list())
	}
}

// The reviewer's case: a lineage stop chooses boss while it runs; boss then starts work in another
// manager (another profile's storage) under its lineage and ends done before the stop lands. That
// work is reached through boss's id; work under another key is left alone.
func TestAStopChosenBeforeARunEndedReachesTheWorkItStartedMeanwhile(t *testing.T) {
	coder, other := NewSubagentManager(), NewSubagentManager()
	t.Cleanup(func() { _ = coder.Close(); _ = other.Close() })
	cascade := func(key, reason string) {
		coder.CancelUnmanagedLineage(keyIs(key), reason)
		other.CancelUnmanagedLineage(keyIs(key), reason)
	}
	coder.SetStopCascade(cascade)
	other.SetStopCascade(cascade)
	parent := withExecutors(t, readOnlyTool("read"))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	finder, err := newSubagentToolAsync(subagentSettings(agentNamed("finder")), parent, nil, blockedLauncher(release, nil), nil, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherCatalog, err := tools.NewCatalog(append(parent, finder))
	if err != nil {
		t.Fatal(err)
	}
	outputs := make(chan string, 3)
	started, proceed := make(chan struct{}), make(chan struct{})
	boss := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		close(started)
		<-proceed
		// Work in another profile keeps boss's lineage, not its cancellation.
		go func() {
			outputs <- subagentCallWith(t, context.WithoutCancel(ctx), otherCatalog, `{"task":"look around","agent":"finder"}`)
		}()
		for deadline := time.Now().Add(5 * time.Second); len(other.liveRuns(func(run *subagentRun) bool { return run.task == "look around" })) == 0; {
			if time.Now().After(deadline) {
				return SubagentResult{}, context.DeadlineExceeded
			}
			time.Sleep(time.Millisecond)
		}
		return SubagentResult{Text: "done"}, nil
	}}
	bossTool, err := newSubagentToolAsync(subagentSettings(agentNamed("boss")), parent, nil, boss, nil, coder, nil)
	if err != nil {
		t.Fatal(err)
	}
	coderCatalog, err := tools.NewCatalog(append(parent, bossTool))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		outputs <- subagentCallWith(t, WithRunLineage(context.Background(), []string{"k2"}), otherCatalog, `{"task":"unrelated","agent":"finder"}`)
	}()
	bossOut := make(chan string, 1)
	go func() {
		bossOut <- subagentCallWith(t, WithRunLineage(context.Background(), []string{"k1"}), coderCatalog, `{"task":"boss","agent":"boss"}`)
	}()
	<-started
	chosen := coder.liveRuns(func(run *subagentRun) bool { return slices.ContainsFunc(run.lineage, keyIs("k1")) })
	if len(chosen) != 1 {
		t.Fatalf("chose %d runs, want boss", len(chosen))
	}
	close(proceed)
	<-bossOut
	bossID := chosen[0].id
	if view := coder.byID(bossID).view(); view.state != subagentRunDone {
		t.Fatalf("boss = %+v, want done before the stop lands", view)
	}
	if n := stopRuns(chosen, "user_stop"); n != 1 {
		t.Fatalf("stopped %d runs, want 1", n)
	}
	var look, unrelated *subagentRun
	eventually(t, "the work boss started to stop", func() bool {
		for _, run := range other.allRuns() {
			switch run.task {
			case "look around":
				look = run
			case "unrelated":
				unrelated = run
			}
		}
		return look != nil && look.view().state == subagentRunStopped
	})
	if view := look.view(); view.reason != "user_stop" {
		t.Fatalf("the work boss started = %+v, want stopped with user_stop", view)
	}
	if unrelated == nil || unrelated.view().state != subagentRunRunning {
		t.Fatalf("work under another key = %+v, want running", unrelated)
	}
	if view := coder.byID(bossID).view(); view.state != subagentRunDone {
		t.Fatalf("boss = %+v, want still done", view)
	}
}

// A stop that chooses a sub-agent handle after it was registered and before its run joined the
// manager finds no run to stop; it is kept and applied once the run exists.
func TestAStopOfAHandleStillStartingIsAppliedOnceItsRunExists(t *testing.T) {
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(release, nil), nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	var chosen atomic.Int64
	manager.afterBegin = func(string) {
		chosen.Store(int64(host.Manager().CancelLineage(keyIs("k1"), "user_stop")))
	}
	id := runIDOf(t, subagentCallWith(t, WithRunLineage(context.Background(), []string{"k1"}), catalog, `{"task":"t","agent":"explore","block_ms":0}`))
	if chosen.Load() != 1 {
		t.Fatalf("the stop chose %d handles, want the starting one", chosen.Load())
	}
	waitHandle(t, host.Manager(), testSessionKey, id, asyncmgr.StateStopped, "user_stop")
	if view := manager.byID(id).view(); view.state != subagentRunStopped || view.reason != "user_stop" {
		t.Fatalf("run = %+v, want stopped with user_stop", view)
	}
}

// A stop of a handle whose run has ended and been dropped since it was chosen still reaches what
// the run started; a stop of a handle still starting is kept for its run and cascades nothing yet.
func TestAStopOfAHandleWhoseRunWasDroppedStillCascades(t *testing.T) {
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	if _, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(make(chan struct{}), nil), nil, manager, host); err != nil {
		t.Fatal(err)
	}
	log := &cascadeLog{}
	manager.SetStopCascade(log.note)
	driver := &subagentDriver{host: host}
	host.beginSubagentStart("sub_starting")
	driver.Stop("sub_starting", "user_stop")
	// The dropped run is reached under the key its handle was registered with, never its bare id.
	driver.StopKeyed("sub_dropped", "subagent:sub_dropped#1", "user_stop")
	driver.Stop("sub_unkeyed", "user_stop")
	if got := log.list(); !slices.Equal(got, []string{"subagent:sub_dropped#1 user_stop"}) {
		t.Fatalf("cascade told %q, want only the dropped run's key", got)
	}
	if reason, stopped := host.endSubagentStart("sub_starting"); !stopped || reason != "user_stop" {
		t.Fatalf("the starting handle kept %q %v, want user_stop", reason, stopped)
	}
}

// HasLiveLineage counts a run under a matching key while it is queued or running, and the run-end
// callback is told, outside every lock, once it ended, when it no longer counts.
func TestRunEndTellsTheEndOfARunUnderALineage(t *testing.T) {
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	release, started := make(chan struct{}), make(chan struct{})
	ended := make(chan bool, 1)
	manager.SetRunEnd(func() { ended <- manager.HasLiveLineage(keyIs("k1")) })
	parent := withExecutors(t, readOnlyTool("read"))
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		close(started)
		<-release
		return SubagentResult{Text: "answer"}, nil
	}}
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, launcher, nil, manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		subagentCallWith(t, WithRunLineage(context.Background(), []string{"k1"}), catalog, `{"task":"t"}`)
	}()
	<-started
	if !manager.HasLiveLineage(keyIs("k1")) || manager.HasLiveLineage(keyIs("k2")) {
		t.Fatal("a running run under k1 is not live under k1 only")
	}
	close(release)
	<-done
	select {
	case live := <-ended:
		if live {
			t.Fatal("the run still counted as live when its end was told")
		}
	case <-time.After(time.Minute):
		t.Fatal("the run's end was not told")
	}
}
