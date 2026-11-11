package native

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// R5c run lineage: the keys a call runs under are recorded on what it starts, and a run's own
// calls run under its lineage with its id added.

func keyIs(key string) func(string) bool { return func(k string) bool { return k == key } }

func TestExtendRunLineageNeverChangesItsInput(t *testing.T) {
	base := make([]string, 1, 4)
	base[0] = "a"
	first := ExtendRunLineage(base, "b")
	second := ExtendRunLineage(base, "c")
	if !slices.Equal(first, []string{"a", "b"}) || !slices.Equal(second, []string{"a", "c"}) || !slices.Equal(base, []string{"a"}) {
		t.Fatalf("first %v second %v base %v", first, second, base)
	}
	if ctx := context.Background(); WithRunLineage(ctx, nil) != ctx || RunLineageOf(ctx) != nil {
		t.Fatal("an empty lineage must leave the context alone")
	}
}

// A sub-agent started under a lineage records it on its handle, runs its own calls under it with
// its id added, and so does its child: a cancel of any key above reaches both handles, and a run
// started outside the lineage is left alone.
func TestRunLineageIsCarriedDownAndRecordedOnHandles(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	leafIDs := make(chan string, 1)
	settings := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
	catalog, subagents, host := nestingFixture(t, settings, map[string]nestedBehaviour{
		"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			mu.Lock()
			seen[request.Execution.ProcessOwner] = RunLineageOf(ctx)
			mu.Unlock()
			if request.Task == "chain" {
				leafIDs <- runIDOf(t, childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf","block_ms":0}`))
			}
			<-ctx.Done()
			return SubagentResult{}, ctx.Err()
		},
		"leaf": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			mu.Lock()
			seen[request.Execution.ProcessOwner] = RunLineageOf(ctx)
			mu.Unlock()
			<-ctx.Done()
			return SubagentResult{}, ctx.Err()
		},
	})
	under := WithRunLineage(context.Background(), []string{"demo\x00dev/coder", "profile_run_7"})
	boss := runIDOf(t, subagentCallWith(t, under, catalog, `{"task":"chain","agent":"boss","block_ms":0}`))
	outside := runIDOf(t, subagentCall(t, catalog, `{"task":"alone","agent":"boss","block_ms":0}`))
	leaf := <-leafIDs
	eventually(t, "every run to have started", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 3
	})
	bossKey, leafKey, outsideKey := subagents.byID(boss).lineageKey, subagents.byID(leaf).lineageKey, subagents.byID(outside).lineageKey
	mu.Lock()
	if got := seen[boss]; !slices.Equal(got, []string{"demo\x00dev/coder", "profile_run_7", bossKey}) {
		t.Errorf("boss runs under %q", got)
	}
	if got := seen[leaf]; !slices.Equal(got, []string{"demo\x00dev/coder", "profile_run_7", bossKey, leafKey}) {
		t.Errorf("leaf runs under %q", got)
	}
	if got := seen[outside]; !slices.Equal(got, []string{outsideKey}) {
		t.Errorf("a run started outside the lineage runs under %q", got)
	}
	mu.Unlock()

	// Both runs are handles: the async manager stops them, the sub-agent manager leaves them.
	if n := subagents.CancelUnmanagedLineage(keyIs("profile_run_7"), "x"); n != 0 {
		t.Fatalf("CancelUnmanagedLineage reached %d handles", n)
	}
	manager := host.Manager()
	if n := manager.CancelLineage(keyIs("profile_run_8"), "x"); n != 0 {
		t.Fatalf("an unrelated key reached %d handles", n)
	}
	// The leaf's handle is the boss's (owner = boss id) and carries boss's key in its lineage;
	// the bare run id is not a lineage key.
	if n := manager.CancelLineage(keyIs(boss), "caller_stopped"); n != 0 {
		t.Fatalf("the boss's bare id reached %d handles", n)
	}
	if n := manager.CancelLineage(keyIs(bossKey), "caller_stopped"); n != 1 {
		t.Fatalf("the boss's key reached %d handles, want its leaf's", n)
	}
	waitHandle(t, manager, boss, leaf, asyncmgr.StateStopped, "caller_stopped")
	if n := manager.CancelLineage(keyIs("demo\x00dev/coder"), "user_stop"); n != 1 {
		t.Fatalf("the session key reached %d live handles, want the boss's", n)
	}
	waitHandle(t, manager, testSessionKey, boss, asyncmgr.StateStopped, "user_stop")
	if entry, err := manager.Status(testSessionKey, outside); err != nil || entry.State != asyncmgr.StateRunning {
		t.Fatalf("the run outside the lineage = %+v %v, want running", entry, err)
	}
}

func waitHandle(t *testing.T, manager *asyncmgr.Manager, owner, id string, state asyncmgr.State, reason string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, err := manager.Status(owner, id)
		if err == nil && entry.State == state && entry.Reason == reason {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle %s = %+v %v, want %s %s", id, entry, err, state, reason)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run that is no handle (a session without the async host) is reached through its lineage by
// CancelUnmanagedLineage, and only when one of its keys matches.
func TestCancelUnmanagedLineageReachesOnlyRunsStartedUnderTheKey(t *testing.T) {
	release := make(chan struct{})
	var cancelled atomic.Int64
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), parent, nil, blockedLauncher(release, &cancelled), nil, manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	// A run without a handle holds its call until it ends, so each call waits in a goroutine.
	outputs := make(chan string, 2)
	for _, key := range []string{"k1", "k2"} {
		go func() {
			outputs <- subagentCallWith(t, WithRunLineage(context.Background(), []string{key}), catalog, `{"task":"`+key+`"}`)
		}()
	}
	runs := map[string]*subagentRun{}
	eventually(t, "both runs to start", func() bool {
		for _, run := range manager.ownedRuns(testSessionKey) {
			runs[run.task] = run
		}
		return len(runs) == 2
	})
	if n := manager.CancelUnmanagedLineage(keyIs("k3"), "x"); n != 0 {
		t.Fatalf("an unrelated key reached %d runs", n)
	}
	if n := manager.CancelUnmanagedLineage(keyIs("k1"), "caller_stopped"); n != 1 {
		t.Fatalf("k1 reached %d runs, want 1", n)
	}
	select {
	case out := <-outputs:
		if runIDOf(t, out) != runs["k1"].id {
			t.Fatalf("the call that ended is not the one under k1:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call under k1 did not end")
	}
	if view := runs["k1"].view(); view.state != subagentRunStopped || view.reason != "caller_stopped" {
		t.Fatalf("the run under k1 = %+v", view)
	}
	if view := runs["k2"].view(); view.state != subagentRunRunning || cancelled.Load() != 1 {
		t.Fatalf("the run under k2 = %+v, cancelled %d", view, cancelled.Load())
	}
	release <- struct{}{}
	<-outputs
}

// The stop cascade is told the id and reason of a run that ended stopped after its launcher ran,
// and of no run that ended on its own or timed out.
func TestStopCascadeIsToldOfStoppedRunsOnly(t *testing.T) {
	type told struct{ key, reason string }
	var mu sync.Mutex
	var calls []told
	release := make(chan struct{})
	timeoutAgent := agentNamed("slow")
	timeoutAgent.TimeoutMS = 30
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	settings := subagentSettings(agentNamed("explore"), timeoutAgent)
	settings.MaxConcurrent = 4
	tool, err := newSubagentToolAsync(settings, parent, nil, blockedLauncher(release, nil), nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	manager.SetStopCascade(func(key, reason string) {
		mu.Lock()
		calls = append(calls, told{key, reason})
		mu.Unlock()
	})
	stopped := runIDOf(t, subagentCall(t, catalog, `{"task":"stop me","agent":"explore","block_ms":0}`))
	done := runIDOf(t, subagentCall(t, catalog, `{"task":"finish","agent":"explore","block_ms":0}`))
	timedOut := runIDOf(t, subagentCall(t, catalog, `{"task":"late","agent":"slow","block_ms":0}`))
	eventually(t, "the slow run to time out", func() bool { return manager.byID(timedOut).view().state == subagentRunTimedOut })
	if out := asyncCall(t, catalog, `{"action":"stop","id":"`+stopped+`"}`); out == "" {
		t.Fatal("no stop result")
	}
	eventually(t, "the stopped run to end", func() bool { return manager.byID(stopped).view().state == subagentRunStopped })
	close(release)
	eventually(t, "the released run to end", func() bool { return manager.byID(done).view().state == subagentRunDone })
	mu.Lock()
	defer mu.Unlock()
	reason := manager.byID(stopped).view().reason
	key := manager.byID(stopped).lineageKey
	if len(calls) != 1 || calls[0].key != key || calls[0].reason != reason || reason == "" {
		t.Fatalf("cascade told %+v, want only %s with its reason %q", calls, key, reason)
	}
}

// An exec handle and a profile_message handle record the lineage of the call that started them.
func TestExecAndProfileMessageHandlesRecordTheCallsLineage(t *testing.T) {
	under := WithRunLineage(context.Background(), []string{"demo\x00dev/coder", "profile_run_3"})

	f := newExecAsyncFixture(t)
	execID := execRunIDOf(t, f.runCtx(under, t, "s1", `{"code":"sleep 30","block_ms":0}`))
	plain := execRunIDOf(t, f.run(t, "s1", `{"code":"sleep 30","block_ms":0}`))
	if n := f.host.Manager().CancelLineage(keyIs("profile_run_3"), "caller_stopped"); n != 1 {
		t.Fatalf("the key reached %d exec handles, want 1", n)
	}
	if entry := f.waitState(t, "s1", execID, asyncmgr.StateStopped); entry.Reason != "caller_stopped" {
		t.Fatalf("reason = %q", entry.Reason)
	}
	if entry, err := f.host.Manager().Status("s1", plain); err != nil || entry.State != asyncmgr.StateRunning {
		t.Fatalf("the exec handle outside the lineage = %+v %v", entry, err)
	}
	f.host.Manager().CancelOwner("s1", "cleanup")

	p := newProfileMessageHandleFixture(t, nil, nil)
	execution := sessionCaller
	execution.CallID = "call"
	out, err := p.message.Execute(under, map[string]any{"profile": "reviewer", "message": "check it", "block_ms": json.Number("0")}, execution)
	if err != nil {
		t.Fatal(err)
	}
	message := handleIDPattern.FindString(out)
	if n := p.host.Manager().CancelLineage(keyIs("demo\x00dev/coder"), "user_stop"); n != 1 {
		t.Fatalf("the key reached %d message handles, want 1", n)
	}
	if reasons := p.messenger.run(t, 0).stopped(); !slices.Equal(reasons, []string{"user_stop"}) {
		t.Fatalf("message %s was asked to stop %v", message, reasons)
	}
}

// Starts under a lineage race with cancels of its key and with the runs' own ends: every run
// started under the key ends stopped once a last cancel follows, a run outside it is never
// reached, and nothing is left behind once the manager closes.
func TestLineageCancelRacesWithStartsAndEnds(t *testing.T) {
	baseline := baselineCount()
	host, err := NewAsyncHost(t.TempDir()+"/.async", asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSubagentManager()
	parent := withExecutors(t, readOnlyTool("read"))
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent, settings.MaxQueued, settings.KeepFinished = 64, 64, 64
	var cancelled atomic.Int64
	release := make(chan struct{})
	tool, err := newSubagentToolAsync(settings, parent, nil, blockedLauncher(release, &cancelled), nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	outside := runIDOf(t, subagentCall(t, catalog, `{"task":"outside","block_ms":0}`))
	under := WithRunLineage(context.Background(), []string{"profile_run_9"})
	var starts sync.WaitGroup
	for range 8 {
		starts.Go(func() {
			for range 4 {
				subagentCallWith(t, under, catalog, `{"task":"under","block_ms":0}`)
			}
		})
	}
	var cancels sync.WaitGroup
	cancels.Go(func() {
		for range 20 {
			host.Manager().CancelLineage(keyIs("profile_run_9"), "caller_stopped")
			manager.CancelUnmanagedLineage(keyIs("profile_run_9"), "caller_stopped")
		}
	})
	starts.Wait()
	cancels.Wait()
	host.Manager().CancelLineage(keyIs("profile_run_9"), "caller_stopped")
	eventually(t, "every run under the key to end", func() bool {
		for _, run := range manager.ownedRuns(testSessionKey) {
			if run.id != outside && run.active() {
				return false
			}
		}
		return true
	})
	if run := manager.byID(outside); !run.active() {
		t.Fatalf("the run outside the key = %+v", run.view())
	}
	// A start past the owner's handle cap is refused and a run cancelled while queued never
	// reaches the launcher; every run that started under the key ended stopped, none done.
	underKey := 0
	for _, run := range manager.ownedRuns(testSessionKey) {
		if run.id == outside {
			continue
		}
		underKey++
		if view := run.view(); view.state != subagentRunStopped {
			t.Errorf("run %s under the key ended %s", run.id, view.state)
		}
	}
	if underKey == 0 || cancelled.Load() > int64(underKey) {
		t.Fatalf("%d runs under the key, %d launcher cancels", underKey, cancelled.Load())
	}
	close(release)
	_ = manager.Close()
	host.Close()
	requireNoGrowth(t, baseline, "lineage cancels racing with starts")
}

// A remote sub-agent handle records the lineage of its call, so a cancel of a key above reaches
// the remote run through the handle's stop. The lineage stays on this server: the start request
// carries no key of it (the remote begins its own tree).
func TestRemoteSubagentHandleRecordsTheCallsLineage(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	execution := remoteCaller
	execution.CallID = "call"
	under := WithRunLineage(context.Background(), []string{"demo\x00dev/coder", "profile_run_4"})
	out, err := f.tool.Execute(under, map[string]any{"task": "find the answer", "agent": remoteAgent, "block_ms": json.Number("0")}, execution)
	if err != nil {
		t.Fatal(err)
	}
	id := remoteRunHandle.FindString(out)
	run := f.peer.backend.run(t, 0)
	if n := f.host.Manager().CancelLineage(keyIs("profile_run_4"), "caller_stopped"); n != 1 || id == "" {
		t.Fatalf("the key reached %d remote handles (id %q)", n, id)
	}
	eventually(t, "the stop to reach the remote run", func() bool { return len(run.stops()) > 0 })
	body, _ := json.Marshal(f.peer.backend.subs[0])
	if strings.Contains(string(body), "profile_run_4") || strings.Contains(string(body), "dev/coder") {
		t.Fatalf("the remote start carries the local lineage: %s", body)
	}
}
