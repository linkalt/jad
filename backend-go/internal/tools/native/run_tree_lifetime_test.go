package native

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Node lifetimes in the run tree (T-007 review): an ended node is counted nowhere and is still a
// parent until it is released; a live descendant keeps its own node whatever happened to its
// parent's; a panic inside finish still takes the run's node out.

// End gives back a node's places and keeps it as a parent: a start below it joins its root under
// its ceiling, it is listed as ended and never counted again, and Release then takes it out, after
// which a start below it fails closed.
func TestAnEndedNodeIsCountedNowhereAndStaysAParentUntilReleased(t *testing.T) {
	var tree RunTree
	root, _ := tree.Reserve(RunReservation{Session: "s", Kind: RunNodeSubagent, State: RunNodeRunning, MaxPerRoot: 2})
	ended, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: "t", Kind: RunNodeProfileMessage, State: RunNodeRunning})
	if err != nil {
		t.Fatal(err)
	}
	ended.End()
	ended.End()
	ended.SetState(RunNodeRunning)
	if got := tree.SessionRuns("t"); got != 0 {
		t.Fatalf("an ended node is still counted in its session: %d", got)
	}
	nodes := tree.Nodes()
	if len(nodes) != 2 || nodes[1].ID != ended.ID() || nodes[1].State != RunNodeEnded {
		t.Fatalf("an ended node must stay listed as ended: %+v", nodes)
	}
	// Its place below the root is free (ceiling 2): two starts below it fit, a third does not.
	first, err := tree.Reserve(RunReservation{Parent: ended.ID(), Session: "t", MaxPerRoot: 8})
	if err != nil {
		t.Fatalf("a start below an ended node: %v", err)
	}
	if _, err := tree.Reserve(RunReservation{Parent: ended.ID(), Session: "t"}); err != nil {
		t.Fatalf("a second start below an ended node: %v", err)
	}
	if _, err := tree.Reserve(RunReservation{Parent: ended.ID(), Session: "t"}); !errors.Is(err, ErrRunTreeLimit) {
		t.Fatalf("a third start below the root's ceiling of 2 = %v", err)
	}
	if info := tree.Nodes()[2]; info.ID != first.ID() || info.Parent != ended.ID() || info.Root != root.ID() || info.RootCap != 2 {
		t.Fatalf("a start below an ended node must join its tree under its ceiling: %+v", info)
	}
	ended.Release()
	ended.Release()
	ended.End()
	if _, err := tree.Reserve(RunReservation{Parent: ended.ID(), Session: "t"}); !errors.Is(err, ErrRunTreeParentGone) {
		t.Fatalf("a start below a released node = %v", err)
	}
	if got := tree.SessionRuns("t"); got != 2 {
		t.Fatalf("releasing an ended node freed another's place: session t counts %d, want 2", got)
	}
}

// A background descendant whose parent's node is gone (a profile_message target's run released
// its transcript while the sub-agent it started as a handle goes on) still starts the children its
// tree allows, under the tree's root and ceiling, is refused past it, and every node goes once the
// runs end.
func TestALiveDescendantStartsChildrenAfterItsParentsNodeIsGone(t *testing.T) {
	settings := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
	settings.MaxDepth = 3
	parentGone, bossStarted, leafRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first, second string
	var snapshot []RunNodeInfo
	bossDone := make(chan struct{})
	var tree *RunTree
	catalog, manager, _ := nestingFixture(t, settings, map[string]nestedBehaviour{
		"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			defer close(bossDone)
			close(bossStarted)
			<-parentGone
			first = childCall(t, ctx, request, "subagent", `{"task":"g1","agent":"leaf","block_ms":0}`)
			snapshot = tree.Nodes()
			second = childCall(t, ctx, request, "subagent", `{"task":"g2","agent":"leaf","block_ms":0}`)
			close(leafRelease)
			return SubagentResult{Text: "boss"}, nil
		},
		"leaf": func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
			select {
			case <-leafRelease:
			case <-ctx.Done():
			}
			return SubagentResult{Text: "leaf"}, nil
		},
	})
	tree = manager.runTree()
	// The caller's tree: a root with a ceiling of 2 and the target's run below it.
	root, _ := tree.Reserve(RunReservation{Session: admissionSeedSession, Kind: RunNodeSubagent, State: RunNodeRunning, MaxPerRoot: 2})
	target, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: testSessionKey, Kind: RunNodeProfileMessage, State: RunNodeRunning})
	if err != nil {
		t.Fatal(err)
	}
	execution := chainExecution(1)
	execution.RunNode = target.ID()
	if out := chainCall(t, catalog, execution, `{"task":"b","agent":"boss","block_ms":0}`); !strings.Contains(out, "sub_") {
		t.Fatalf("the target's background start: %s", out)
	}
	awaitSignal(t, bossStarted, "boss to start")
	target.Release()
	close(parentGone)
	awaitSignal(t, bossDone, "boss to end")
	if !strings.Contains(first, "sub_") || strings.Contains(first, "subagent_unavailable") {
		t.Fatalf("a live descendant whose parent's node is gone could not start a child: %s", first)
	}
	if !strings.Contains(second, "subagent_limit_reached") {
		t.Fatalf("its second child is past the tree's ceiling of 2 (boss and the first): %s", second)
	}
	var boss, leaf RunNodeInfo
	for _, node := range snapshot {
		if node.Parent == target.ID() {
			boss = node
		}
	}
	for _, node := range snapshot {
		if boss.ID != "" && node.Parent == boss.ID {
			leaf = node
		}
	}
	if boss.ID == "" || boss.Root != root.ID() || leaf.Parent != boss.ID || leaf.Root != root.ID() || leaf.RootCap != 2 {
		t.Fatalf("the descendant and its child must stay in the caller's tree: %+v", snapshot)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(tree.Nodes()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if nodes := tree.Nodes(); len(nodes) != 1 || nodes[0].ID != root.ID() {
		t.Fatalf("after the runs ended only the seeded root may be left: %+v", nodes)
	}
	if got := tree.SessionRuns(testSessionKey); got != 0 {
		t.Fatalf("the session still counts %d runs", got)
	}
	root.Release()
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A panic inside finish, after the run's end was saved and before its node left the tree, is
// caught by recoverRun, which takes the node out: the call returns, nothing is left counted, and a
// later start is admitted and finishes.
func TestAPanicInsideFinishGivesBackTheRunsNode(t *testing.T) {
	answer := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "ok"}, nil
	}}
	catalog, manager := releaseFixture(t, subagentSettings(agentNamed("explore")), answer)
	var panicked atomic.Bool
	manager.inFinish = func() {
		if panicked.CompareAndSwap(false, true) {
			panic("finish exploded")
		}
	}
	if _, returned := callReturnsWithin(t, catalog, `{"task":"x"}`, 5*time.Second); !returned {
		t.Fatal("the call never returned")
	}
	if !panicked.Load() {
		t.Fatal("finish did not run the panic")
	}
	awaitNoLiveTreeRuns(t, manager, "panic inside finish")
	if out := subagentCall(t, catalog, `{"task":"y"}`); !strings.Contains(out, "finished") {
		t.Fatalf("a start after the panic: %s", out)
	}
	awaitNoLiveTreeRuns(t, manager, "the start after the panic")
}

// A background writer whose finish panics after its end was saved still ends whole: its async
// handle is terminal with the recorded answer, its writer place, slot and node are given back, it
// is in the finished list, and no goroutine is left; a later handoff of the same session runs.
func TestAPanicInsideFinishOfABackgroundRunEndsItsHandleAndGivesEverythingBack(t *testing.T) {
	baseline := baselineCount()
	release := make(chan struct{})
	catalog, manager, host := backgroundCatalog(t, backgroundWriterSettings(1, writerAgent("fixer", "read", "exec")), writingLauncher(release, nil), readOnlyTool("read"), writeTool("exec"))
	var panicked atomic.Bool
	manager.inFinish = func() {
		if panicked.CompareAndSwap(false, true) {
			panic("finish exploded")
		}
	}
	out, returned := callReturnsWithin(t, catalog, `{"task":"fix","block_ms":0}`, 2*time.Second)
	if !returned {
		close(release)
		t.Fatal("the writer was not handed off")
	}
	id := runIDOf(t, out)
	close(release)
	got := asyncWait(t, catalog)
	for _, want := range []string{id, "done", "fixed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the handle of a run whose finish panicked is not terminal with its answer (lacks %q):\n%s", want, got)
		}
	}
	if !panicked.Load() {
		t.Fatal("finish did not run the panic")
	}
	eventually(t, "the writer place to be given back", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
	awaitNoLiveTreeRuns(t, manager, "panic inside a background run's finish")
	eventually(t, "the slot to be given back", func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.running == 0
	})
	manager.mu.Lock()
	listed := slices.Contains(manager.finished, id)
	manager.mu.Unlock()
	if !listed {
		t.Fatal("the run is not in the finished list, so keep_finished never trims it")
	}
	second := runIDOf(t, subagentCall(t, catalog, `{"task":"again","block_ms":0}`))
	if got := asyncWait(t, catalog); !strings.Contains(got, second) || !strings.Contains(got, "done") {
		t.Fatalf("a handoff after the panic:\n%s", got)
	}
	eventually(t, "the second writer place to be given back", func() bool { return manager.BackgroundWriters(testSessionKey) == 0 })
	awaitNoLiveTreeRuns(t, manager, "the handoff after the panic")
	_ = manager.Close()
	host.Close()
	requireNoGrowth(t, baseline, "a background run whose finish panicked")
}
