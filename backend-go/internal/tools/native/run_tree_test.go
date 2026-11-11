package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// The run tree on its own (T-007): ids, parent and root, both caps, the ceiling, and a release
// that counts once.
func TestRunTreeCountsBelowTheRootAndPerSessionAndReleasesOnce(t *testing.T) {
	var tree RunTree
	root, err := tree.Reserve(RunReservation{Session: "s", Kind: RunNodeSubagent, State: RunNodeRunning, MaxPerRoot: 2})
	if err != nil {
		t.Fatal(err)
	}
	child, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: "t", Kind: RunNodeProfileMessage, State: RunNodeQueued, MaxPerRoot: 8})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := tree.Reserve(RunReservation{Parent: child.ID(), Session: "t", Kind: RunNodeSubagent, State: RunNodeQueued, MaxPerRoot: 8, MaxPerSession: 2})
	if err != nil {
		t.Fatal(err)
	}
	nodes := tree.Nodes()
	if len(nodes) != 3 || nodes[1].Parent != root.ID() || nodes[2].Parent != child.ID() || nodes[2].Root != root.ID() || nodes[1].Kind != RunNodeProfileMessage {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[1].RootCap != 2 || nodes[2].RootCap != 2 {
		t.Fatalf("a caller that allows more widened the root's ceiling: %+v", nodes)
	}
	// Two below the root: the ceiling of 2 is reached whatever the caller allows.
	if _, err := tree.Reserve(RunReservation{Parent: grandchild.ID(), Session: "u", MaxPerRoot: 100}); !errors.Is(err, ErrRunTreeLimit) {
		t.Fatalf("a start past the root's ceiling = %v", err)
	}
	// The session cap is the executing session's own value (MaxPerSession), on that session's count.
	child.SetState(RunNodeRunning)
	grandchild.Release()
	if _, err := tree.Reserve(RunReservation{Parent: child.ID(), Session: "t", MaxPerSession: 1}); !errors.Is(err, ErrRunTreeLimit) {
		t.Fatalf("a start in a session at the caller's cap = %v", err)
	}
	// A root is counted but never refused.
	if _, err := tree.Reserve(RunReservation{Session: "t", MaxPerSession: 1}); err != nil {
		t.Fatalf("a root was refused: %v", err)
	}
	if _, err := tree.Reserve(RunReservation{Parent: grandchild.ID(), Session: "u"}); !errors.Is(err, ErrRunTreeParentGone) {
		t.Fatalf("a start below a released node = %v", err)
	}
	grandchild.Release()
	child.Release()
	child.Release()
	if got := tree.SessionRuns("t"); got != 1 {
		t.Fatalf("session t counts %d runs, want the root started there (a double release freed another's place)", got)
	}
	// The root's own node going leaves the count below it to its children: a run below it still
	// counts, and the ceiling still holds for it.
	below, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	root.Release()
	if _, err := tree.Reserve(RunReservation{Parent: below.ID(), Session: "s"}); err != nil {
		t.Fatalf("one below a root whose run ended (ceiling 2): %v", err)
	}
	if _, err := tree.Reserve(RunReservation{Parent: below.ID(), Session: "s"}); !errors.Is(err, ErrRunTreeLimit) {
		t.Fatalf("a third below a root whose run ended = %v", err)
	}
	var nilNode *RunNode
	nilNode.Release()
	nilNode.SetState(RunNodeRunning)
	if nilNode.ID() != "" {
		t.Fatal("a nil node has an id")
	}
}

// Concurrent starts in different managers sharing one tree never both take its last place, and
// every node goes once the runs end.
func TestRunTreeLastPlaceIsTakenOnceUnderConcurrentStarts(t *testing.T) {
	var tree RunTree
	root, err := tree.Reserve(RunReservation{Session: "s", MaxPerRoot: 5})
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	nodes := make(chan *RunNode, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if node, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: "s"}); err == nil {
				admitted.Add(1)
				nodes <- node
			}
		}()
	}
	wg.Wait()
	close(nodes)
	if admitted.Load() != 5 {
		t.Fatalf("%d starts were admitted below a ceiling of 5", admitted.Load())
	}
	for node := range nodes {
		wg.Add(2)
		go func() { defer wg.Done(); node.Release() }()
		go func() { defer wg.Done(); node.Release() }()
	}
	wg.Wait()
	root.Release()
	if left := tree.Nodes(); len(left) != 0 {
		t.Fatalf("nodes left: %+v", left)
	}
}

// Two managers that give their runs the same id share the resolver's tree without mixing their
// trees: each run is its own root, so each root cap counts only its own children, while the
// session cap counts the session's runs of both managers.
func TestManagersWithTheSameRunIDsShareOneTree(t *testing.T) {
	var tree RunTree
	settings := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
	settings.MaxRunsPerRoot, settings.MaxRunsPerSession = 1, 4
	release := make(chan struct{})
	held := make(chan struct{}, 4)
	var outputs sync.Map
	var calls sync.WaitGroup
	build := func(name string) *SubagentManager {
		catalog, manager, _ := nestingFixture(t, settings, map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				var replies []string
				for range 2 {
					replies = append(replies, childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf","block_ms":0}`))
				}
				outputs.Store(name, replies)
				held <- struct{}{}
				<-release
				return SubagentResult{Text: "boss done"}, nil
			},
			"leaf": func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
				<-ctx.Done()
				return SubagentResult{}, ctx.Err()
			},
		})
		manager.SetRunTree(&tree)
		// Each manager numbers its runs from 1, so both have a sub_00000001 and a sub_00000002.
		var next atomic.Int32
		manager.newRunID = func() (string, error) { return fmt.Sprintf("sub_%08d", next.Add(1)), nil }
		calls.Add(1)
		go func() {
			defer calls.Done()
			subagentCall(t, catalog, `{"task":"x","agent":"boss","block_ms":0}`)
		}()
		return manager
	}
	first := build("first")
	<-held
	second := build("second")
	<-held
	for _, name := range []string{"first", "second"} {
		replies, _ := outputs.Load(name)
		list := replies.([]string)
		if !strings.Contains(list[0], "is still running") || !strings.Contains(list[1], "subagent_limit_reached") {
			t.Fatalf("%s manager: want one child below its own root, then the root cap:\n%s", name, strings.Join(list, "\n--\n"))
		}
	}
	if first.byID("sub_00000001") == nil || second.byID("sub_00000001") == nil {
		t.Fatal("the two managers did not both hold a run sub_00000001")
	}
	// Two bosses and two leaves, in one session, across the two managers.
	if got := tree.SessionRuns(testSessionKey); got != 4 {
		t.Fatalf("the shared tree counts %d runs of the session, want 4", got)
	}
	close(release)
	calls.Wait()
	_ = first.Close()
	_ = second.Close()
	if left := tree.Nodes(); len(left) != 0 {
		t.Fatalf("nodes left after both managers closed: %+v", left)
	}
}

// Behaviour change (T-007): a start from a profile_message target that a run started carries the
// target's node, so it is a nested start of that run's tree: the run caps count it, under the
// tree's ceiling (contrast TestAStartWithNoRunNodeIsGatedAndFailsFastButNotCapped).
func TestAStartFromAProfileTargetIsCappedInItsCallersTree(t *testing.T) {
	var launched atomic.Int32
	catalog, manager, _ := nestingFixture(t, admissionSettings(), map[string]nestedBehaviour{
		"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
			launched.Add(1)
			return SubagentResult{Text: "leaf"}, nil
		},
	})
	tree := manager.runTree()
	root, _ := tree.Reserve(RunReservation{Session: admissionSeedSession, MaxPerRoot: 1})
	target, err := tree.Reserve(RunReservation{Parent: root.ID(), Session: testSessionKey, Kind: RunNodeProfileMessage, State: RunNodeRunning})
	if err != nil {
		t.Fatal(err)
	}
	execution := chainExecution(1)
	execution.RunNode = target.ID()
	if out := chainCall(t, catalog, execution, `{"task":"t","agent":"leaf","block_ms":0}`); !strings.Contains(out, "subagent_limit_reached") || launched.Load() != 0 {
		t.Fatalf("a target's start past its tree's ceiling of 1: %s", out)
	}
	if got := tree.SessionRuns(testSessionKey); got != 1 {
		t.Fatalf("the refused start left a node: the session counts %d runs, want the target's own", got)
	}
}

// A call from inside a child (AgentDepth above 0) that carries no node, or a node that is not live,
// is refused instead of starting an uncounted tree, locally and remotely, and starts nothing.
func TestANestedStartWithoutALiveNodeFailsClosed(t *testing.T) {
	var launched atomic.Int32
	catalog, manager, _ := nestingFixture(t, admissionSettings(), map[string]nestedBehaviour{
		"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
			launched.Add(1)
			return SubagentResult{Text: "leaf"}, nil
		},
	})
	for name, node := range map[string]string{"no node": "", "a node that is not live": "run_999999"} {
		execution := chainExecution(0)
		execution.AgentDepth, execution.RootRun, execution.RunNode = 1, "sub_root", node
		if out := chainCall(t, catalog, execution, `{"task":"t","agent":"leaf"}`); !strings.Contains(out, "subagent_unavailable") || !strings.Contains(out, "is not going any more") {
			t.Errorf("%s: %s", name, out)
		}
	}
	if launched.Load() != 0 || len(manager.runTree().Nodes()) != 0 {
		t.Fatalf("a refused start ran (%d) or left nodes: %+v", launched.Load(), manager.runTree().Nodes())
	}
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) { s.MaxDepth = 3 })
	nested := remoteCaller
	nested.AgentDepth, nested.RootRun, nested.RunNode = 1, "sub_00000001", "run_999999"
	if out := f.start(t, remoteAgent, 0, nested); !strings.Contains(out, "subagent_unavailable") || len(f.peer.backend.subs) != 0 {
		t.Fatalf("a remote start from a node that is not live reached the peer:\n%s", out)
	}
}
