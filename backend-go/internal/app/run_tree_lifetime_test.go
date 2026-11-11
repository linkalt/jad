package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// T-007 shared run tree review: the session cap is the executing session's own value across a
// profile hop, a message that ends while its start is on its way leaves no node, and an ended
// message run stays the parent of the work it left going.

// A profile_message run executes in its target's session, so the target's own
// max_runs_per_session bounds it, not its caller's: one run of another tree already executes in
// the reviewer's session, and the caller's and the target's caps differ.
func TestRunTreeSessionCapIsTheExecutingSessionsOwnAcrossAProfileHop(t *testing.T) {
	for _, tc := range []struct {
		name               string
		coder, reviewer    string
		wantFinder         int32
		wantMessageRefused bool
	}{
		{"the target's own lower cap refuses the message", `"max_runs_per_session":8,`, `"max_runs_per_session":1,`, 0, true},
		{"the target's own higher cap admits it past the caller's", `"max_runs_per_session":1,`, `"max_runs_per_session":8,`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mixedCapsFixture(t, tc.coder)
			f.overwrite(t, "tools/coding/review.json", mixedChainToolset("", `"max_depth":3,`+tc.reviewer))
			f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/review"}`)
			r := f.service.resolver
			reference, err := r.CanonicalSessionReference("local:demo")
			if err != nil {
				t.Fatal(err)
			}
			other, err := r.runTree.Reserve(native.RunReservation{Session: sessionKey(reference, "review/reviewer"), Kind: native.RunNodeSubagent, State: native.RunNodeRunning})
			if err != nil {
				t.Fatal(err)
			}
			transcripts, finder, _ := mixedDepthRun(t, f, mixedChainScript)
			if finder != tc.wantFinder || strings.Contains(transcripts, "profile_limit_reached") != tc.wantMessageRefused || strings.Contains(transcripts, "subagent_limit_reached") {
				t.Fatalf("finder sent %d requests (want %d), message refused %v (want %v):\n%s", finder, tc.wantFinder,
					strings.Contains(transcripts, "profile_limit_reached"), tc.wantMessageRefused, transcripts)
			}
			other.Release()
			awaitEmptyRunTree(t, r)
		})
	}
}

// stopDuringStart is the resolver's profile run starter with a hook that runs once a waiting
// (pumped) start has started the target run and before it returns.
type stopDuringStart struct {
	httpapi.ProfileRunStarter
	hook func()
	ran  atomic.Bool
}

func (s *stopDuringStart) StartProfileRun(ctx context.Context, request httpapi.ProfileRunRequest) (*httpapi.ProfileRun, error) {
	run, err := s.ProfileRunStarter.StartProfileRun(ctx, request)
	if err == nil && request.Wait && s.ran.CompareAndSwap(false, true) {
		s.hook()
	}
	return run, err
}

// A queued message that is stopped, and so finishes, while the pump is starting its target run
// keeps its node until that start ended, and attach (seeing the message finished) stops the run and
// gives the node back once the run released the target's transcript: nothing is left in the tree
// and the target is free.
func TestAMessageThatEndsWhileItsStartIsOnItsWayLeavesNoNode(t *testing.T) {
	p := newPMFixture(t, nil)
	r := p.f.service.resolver
	release := p.holdTranscript(t)
	_, run, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1", MaxQueued: 4})
	if err != nil || !run.Queued() {
		t.Fatalf("start = %v, queued %v", err, run != nil && run.Queued())
	}
	var during []native.RunNodeInfo
	starter := &stopDuringStart{ProfileRunStarter: r.profileRunStarter()}
	starter.hook = func() {
		run.Stop("caller_cancelled")
		<-run.Done()
		during = pmNodes(p)
	}
	r.SetProfileRuns(starter)
	release()
	awaitSignal(t, run.Done(), "the message to finish")
	awaitEmptyRunTree(t, r)
	if !starter.ran.Load() {
		t.Fatal("the pump's start never ran the hook")
	}
	if len(during) != 1 {
		t.Fatalf("a message that finished while its start was on its way left the tree before the start ended: %+v", during)
	}
	if outcome := run.Outcome(); outcome.Err == nil {
		t.Fatalf("the stopped message must not succeed: %+v", outcome)
	}
	held := p.holdTranscript(t)
	held()
}

// An ended message run stays in the tree, counted nowhere, while work it left goes under its
// lineage key (a target's handle outlives its run): a start that work makes afterwards (a
// tool_smith script's profile_message carries the run's node) joins the run's tree below it
// instead of failing closed, and the node goes once that work ended.
func TestAnEndedMessageRunStaysTheParentOfTheWorkItLeft(t *testing.T) {
	p := newPMFixture(t, nil)
	r := p.f.service.resolver
	host, err := r.AsyncHost(native.AsyncHostConfig{Dir: t.TempDir(), Config: asyncmgr.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	manager := host.Manager()
	if err := manager.AddDriver(heldDriver{}); err != nil {
		t.Fatal(err)
	}
	var registered atomic.Bool
	r.runnerReturned = func(ctx context.Context) {
		if registered.CompareAndSwap(false, true) {
			if err := manager.Register(asyncmgr.Registration{ID: "background", Owner: "elsewhere", Kind: "held_work", Lineage: native.RunLineageOf(ctx)}); err != nil {
				t.Error(err)
			}
		}
	}
	if got := p.wait(t, p.handle(t, p.send(t, "alpha-1"))); !strings.Contains(got, `"state":"done"`) {
		t.Fatalf("the message did not complete:\n%s", got)
	}
	reference, err := r.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	reviewer := sessionKey(reference, "review/reviewer")
	var ended native.RunNodeInfo
	pollFor(t, "the message's node to end", func() bool {
		nodes := pmNodes(p)
		if len(nodes) == 1 && nodes[0].State == native.RunNodeEnded {
			ended = nodes[0]
			return true
		}
		return false
	})
	if got := r.runTree.SessionRuns(reviewer); got != 0 {
		t.Fatalf("an ended message run is still counted in its target's session: %d", got)
	}

	// The handle's late message, from the reviewer's session with the ended run's node.
	holdCoder, err := r.LockSession(context.Background(), "local:demo", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := profileMessenger{resolver: r}.Prepare(p.callerCtx(t, "review/reviewer"), native.ProfileMessageRequest{
		OwnerKey: reviewer, Target: "dev/coder", Message: "alpha-2", MaxQueued: 4, MaxRunsPerRoot: 16,
		Caller: tools.CallerContext{RunNode: ended.ID, ChainDepth: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	late, err := plan.Start(context.Background())
	if err != nil {
		holdCoder()
		t.Fatalf("a start from the work an ended run left = %v, want it admitted below that run", err)
	}
	var queued native.RunNodeInfo
	for _, node := range pmNodes(p) {
		if node.ID != ended.ID {
			queued = node
		}
	}
	if queued.Parent != ended.ID || queued.Root != ended.Root || queued.RootCap != ended.RootCap || queued.State != native.RunNodeQueued {
		holdCoder()
		t.Fatalf("the late message must join the ended run's tree below it: ended %+v, late %+v", ended, queued)
	}
	holdCoder()
	awaitSignal(t, late.Done(), "the late message to end")
	if outcome := late.Outcome(); outcome.Err != nil {
		t.Fatalf("late message: %+v", outcome)
	}
	// The late message's own node goes once its run released the coder's transcript, just after
	// Done; the ended run stays while its work goes.
	pollFor(t, "only the ended run to be left", func() bool {
		nodes := pmNodes(p)
		return len(nodes) == 1 && nodes[0].ID == ended.ID
	})
	if nodes := pmNodes(p); len(nodes) != 1 || nodes[0].State != native.RunNodeEnded {
		t.Fatalf("while its work goes only the ended run may be left: %+v", nodes)
	}
	if _, err := manager.Complete("background", asyncmgr.Result{State: asyncmgr.StateDone}); err != nil {
		t.Fatal(err)
	}
	awaitEmptyRunTree(t, r)
	r.nodeDrains.mu.Lock()
	pending := len(r.nodeDrains.pending)
	r.nodeDrains.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d ended nodes still wait for a drain", pending)
	}
}
