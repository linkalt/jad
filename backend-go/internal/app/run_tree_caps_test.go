package app

import (
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// T-007 shared run tree (design §3.5, §3.7). Every sub-agent manager and every local
// profile_message run are nodes of the resolver's one run tree, so max_runs_per_root and
// max_runs_per_session count both kinds of run across a profile hop and across managers.
//
// Behaviour change, isolated in the tests marked so: before the tree, a profile hop started a
// tree of its own (its target's sub-agents were top-level starts, never checked against the
// caps) and a message run was counted nowhere. In the mixed chain dev/coder -> explore ->
// review/reviewer -> finder, explore is a top-level run, so it is the root; the reviewer's
// message run and finder hang below it.

// mixedCapsRun runs the mixed chain at max_depth 3 with the subagent section keys extra, and
// checks that the tree is empty once the chain has ended.
func mixedCapsRun(t *testing.T, f *layoutFixture) (string, int32) {
	t.Helper()
	transcripts, finder, _ := mixedDepthRun(t, f, mixedChainScript)
	awaitEmptyRunTree(t, f.service.resolver)
	return transcripts, finder
}

func mixedCapsFixture(t *testing.T, extra string) *layoutFixture {
	t.Helper()
	f := newLayoutFixture(t)
	f.overwrite(t, "tools/coding/none.json", mixedChainToolset("", `"max_depth":3,`+extra))
	return f
}

func awaitEmptyRunTree(t *testing.T, r *Resolver) {
	t.Helper()
	pollFor(t, "an empty run tree", func() bool { return len(r.runTree.Nodes()) == 0 })
}

// Behaviour change: finder is the second run below explore, past max_runs_per_root 1 (before
// the tree it started a tree of its own and ran).
func TestRunTreeRootCapAcrossAProfileHop(t *testing.T) {
	transcripts, finder := mixedCapsRun(t, mixedCapsFixture(t, `"max_runs_per_root":1,`))
	if finder != 0 || !strings.Contains(transcripts, "subagent_limit_reached") {
		t.Fatalf("finder sent %d requests; want it refused subagent_limit_reached:\n%s", finder, transcripts)
	}
	if strings.Contains(transcripts, "profile_limit_reached") {
		t.Fatalf("the message itself was the first run below explore and must pass:\n%s", transcripts)
	}
}

// One more place admits finder: the cap counts the message run and finder, not the root.
func TestRunTreeRootCapAdmitsTheTargetsChildBelowIt(t *testing.T) {
	transcripts, finder := mixedCapsRun(t, mixedCapsFixture(t, `"max_runs_per_root":2,`))
	if finder != 1 || strings.Contains(transcripts, "limit_reached") {
		t.Fatalf("finder sent %d requests (want 1):\n%s", finder, transcripts)
	}
}

// Behaviour change: the session cap counts the session that executes a run. The reviewer's
// message run executes in the reviewer's session, so finder (also there) is its second run past
// max_runs_per_session 1. Before the tree the message run was counted nowhere.
func TestRunTreeSessionCapCountsTheMessageRunInItsTargetsSession(t *testing.T) {
	transcripts, finder := mixedCapsRun(t, mixedCapsFixture(t, `"max_runs_per_session":1,`))
	if finder != 0 || !strings.Contains(transcripts, "subagent_limit_reached") {
		t.Fatalf("finder sent %d requests; want it refused subagent_limit_reached:\n%s", finder, transcripts)
	}
}

// The root's max_runs_per_root is the tree's ceiling: a target whose own config allows more does
// not widen it, and a caller below can narrow it.
func TestRunTreeRootCapIsACeilingAcrossDifferingSettings(t *testing.T) {
	for _, tc := range []struct {
		name               string
		coder, reviewer    string
		wantFinder         int32
		wantLimitInReplies bool
	}{
		{"the root's lower cap is kept by a target that allows more", `"max_runs_per_root":1,`, `"max_runs_per_root":8,`, 0, true},
		{"a target narrows the root's higher cap", `"max_runs_per_root":8,`, `"max_runs_per_root":1,`, 0, true},
		{"both allow two", `"max_runs_per_root":2,`, `"max_runs_per_root":8,`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mixedCapsFixture(t, tc.coder)
			f.overwrite(t, "tools/coding/review.json", mixedChainToolset("", `"max_depth":3,`+tc.reviewer))
			f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large"],"toolset":"coding/review"}`)
			transcripts, finder := mixedCapsRun(t, f)
			if finder != tc.wantFinder || strings.Contains(transcripts, "subagent_limit_reached") != tc.wantLimitInReplies {
				t.Fatalf("finder sent %d requests (want %d):\n%s", finder, tc.wantFinder, transcripts)
			}
		})
	}
}

// pmNodes is the message nodes of the fixture's tree.
func pmNodes(p *pmFixture) []native.RunNodeInfo {
	var nodes []native.RunNodeInfo
	for _, node := range p.f.service.resolver.runTree.Nodes() {
		if node.Kind == native.RunNodeProfileMessage {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func (p *pmFixture) startPlan(t *testing.T, request native.ProfileMessageRequest) (native.ProfileMessagePlan, native.ProfileMessageRun, error) {
	t.Helper()
	request.OwnerKey, request.Target = p.owner, "review/reviewer"
	plan, err := profileMessenger{resolver: p.f.service.resolver}.Prepare(p.callerCtx(t, "dev/coder"), request)
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(p.callerCtx(t, "dev/coder"))
	return plan, run, err
}

// A message run is a node from its admission to its end on every path, queued or running, and
// leaves the tree once the message ended and its target released the transcript.
func TestProfileMessageRunHoldsItsTreeNodeUntilItEnds(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		p := newPMFixture(t, nil)
		if got := p.wait(t, p.handle(t, p.send(t, "alpha-1"))); !strings.Contains(got, `"state":"done"`) {
			t.Fatalf("the message did not complete:\n%s", got)
		}
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("queued, then stopped", func(t *testing.T) {
		p := newPMFixture(t, nil)
		release := p.holdTranscript(t)
		defer release()
		plan, run, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1", MaxQueued: 4})
		if err != nil || !run.Queued() {
			t.Fatalf("start = %v, queued %v", err, run != nil && run.Queued())
		}
		nodes := pmNodes(p)
		if len(nodes) != 1 || nodes[0].State != native.RunNodeQueued || nodes[0].Session != plan.(*profileMessagePlan).key || nodes[0].Parent != "" || nodes[0].Root != nodes[0].ID {
			t.Fatalf("a queued message from a user's run must be a queued root executed in the target's session: %+v", nodes)
		}
		run.Stop("caller_cancelled")
		awaitSignal(t, run.Done(), "the stopped message to finish")
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("queued behind the head, then stopped", func(t *testing.T) {
		p := newPMFixture(t, nil)
		release := p.holdTranscript(t)
		defer release()
		_, head, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1", MaxQueued: 4})
		if err != nil {
			t.Fatal(err)
		}
		_, behind, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-2", MaxQueued: 4})
		if err != nil {
			t.Fatal(err)
		}
		if nodes := pmNodes(p); len(nodes) != 2 {
			t.Fatalf("two queued messages hold %d nodes", len(nodes))
		}
		behind.Stop("caller_cancelled")
		awaitSignal(t, behind.Done(), "the message behind the head to finish")
		pollFor(t, "only the head's node", func() bool { return len(pmNodes(p)) == 1 })
		head.Stop("caller_cancelled")
		awaitSignal(t, head.Done(), "the head to finish")
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("queued, then run", func(t *testing.T) {
		p := newPMFixture(t, nil)
		release := p.holdTranscript(t)
		_, run, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1", MaxQueued: 4})
		if err != nil || !run.Queued() {
			t.Fatalf("start = %v", err)
		}
		release()
		awaitSignal(t, run.Started(), "the queued message to start")
		awaitSignal(t, run.Done(), "the queued message to end")
		if outcome := run.Outcome(); outcome.Err != nil {
			t.Fatalf("outcome = %+v", outcome)
		}
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("queue wait ended", func(t *testing.T) {
		p := newPMFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.BusyWaitMs = 1 })
		release := p.holdTranscript(t)
		defer release()
		if got := p.wait(t, p.handle(t, p.send(t, "alpha-1"))); !strings.Contains(got, "profile_busy") {
			t.Fatalf("the message must expire with profile_busy:\n%s", got)
		}
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("busy, no queue", func(t *testing.T) {
		p := newPMFixture(t, nil)
		release := p.holdTranscript(t)
		defer release()
		if _, _, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1"}); !errors.Is(err, native.ErrProfileBusy) {
			t.Fatalf("start = %v, want busy", err)
		}
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
	t.Run("full line", func(t *testing.T) {
		p := newPMFixture(t, nil)
		release := p.holdTranscript(t)
		_, first, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-1", MaxQueued: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.startPlan(t, native.ProfileMessageRequest{Message: "alpha-2", MaxQueued: 1}); !errors.Is(err, native.ErrProfileBusy) {
			t.Fatalf("second start = %v, want busy", err)
		}
		if nodes := pmNodes(p); len(nodes) != 1 {
			t.Fatalf("the refused message left a node: %+v", nodes)
		}
		first.Stop("caller_cancelled")
		release()
		awaitSignal(t, first.Done(), "the first message to end")
		awaitEmptyRunTree(t, p.f.service.resolver)
	})
}

// seedTree puts a root executed in session in the resolver's tree with the given ceiling, and
// below more runs under it, and returns the root's node id.
func seedTree(t *testing.T, r *Resolver, session string, ceiling, below int) string {
	t.Helper()
	root, err := r.runTree.Reserve(native.RunReservation{Session: session, Kind: native.RunNodeSubagent, State: native.RunNodeRunning, MaxPerRoot: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	for range below {
		if _, err := r.runTree.Reserve(native.RunReservation{Parent: root.ID(), Session: session, Kind: native.RunNodeSubagent, State: native.RunNodeRunning}); err != nil {
			t.Fatal(err)
		}
	}
	return root.ID()
}

// A message from inside a run whose tree is full is refused before anything is queued or run,
// with a clear profile_limit_reached text, and leaves no node; the root's ceiling (1) holds
// although the caller's own root cap is the default 16.
func TestProfileMessageIsRefusedAtTheRootCeilingOfItsCallersTree(t *testing.T) {
	p := newPMFixture(t, nil)
	root := seedTree(t, p.f.service.resolver, "other\x00session", 1, 1)
	before := len(p.f.service.resolver.runTree.Nodes())
	out := p.sendAs(t, p.callerCtx(t, "dev/coder"), tools.ExecutionContext{SessionKey: p.owner, RunNode: root, ChainDepth: 1}, "alpha-1")
	if !strings.Contains(out, "profile_limit_reached") || !strings.Contains(out, "max_runs_per_root") {
		t.Fatalf("want profile_limit_reached:\n%s", out)
	}
	if served := p.order.served(); len(served) != 0 {
		t.Fatalf("the refused message ran: %v", served)
	}
	if after := len(p.f.service.resolver.runTree.Nodes()); after != before {
		t.Fatalf("the refused message left a node: %d nodes, want %d", after, before)
	}
}

// The session cap counts the target's session across trees, under the target's own
// max_runs_per_session (read from the reviewer's settings; 32 without a subagent section);
// a message of a user's run (a root) is counted but never refused; a stale caller node fails
// closed.
func TestProfileMessageSessionCapAndParentAcrossTrees(t *testing.T) {
	p := newPMFixture(t, nil)
	r := p.f.service.resolver
	reference, err := r.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	target := sessionKey(reference, "review/reviewer")
	limit := int(boundRunnerOf(t, p.f, "review/reviewer").config.NativeSubagent.RunMaxRunsPerSession())
	for range limit {
		seedTree(t, r, target, 0, 0) // other trees' runs executing in the target's session
	}
	caller := seedTree(t, r, "other\x00session", 0, 0)
	request := native.ProfileMessageRequest{Message: "alpha-1", Caller: tools.CallerContext{RunNode: caller, ChainDepth: 1}}
	if _, _, err := p.startPlan(t, request); !errors.Is(err, native.ErrRunTreeLimit) {
		t.Fatalf("a nested message to a session at its own cap of %d = %v, want ErrRunTreeLimit", limit, err)
	}
	request.Caller.RunNode = "run_999999"
	if _, _, err := p.startPlan(t, request); !errors.Is(err, native.ErrRunTreeParentGone) {
		t.Fatalf("a message from a node that is not live = %v, want ErrRunTreeParentGone", err)
	}
	request.Caller = tools.CallerContext{}
	_, run, err := p.startPlan(t, request)
	if err != nil {
		t.Fatalf("a message of a user's run is a root and is not checked: %v", err)
	}
	awaitSignal(t, run.Done(), "the root message to end")
	pollFor(t, "the seeded nodes only", func() bool { return len(r.runTree.Nodes()) == limit+1 })
	if sessions := r.runTree.SessionRuns(target); sessions != limit {
		t.Fatalf("the target's session counts %d runs, want the %d seeded", sessions, limit)
	}
}

// A caller without a tools.native.subagent section admits its message under the default caps
// (max_runs_per_root 16): the 17th run below a root without a ceiling of its own is refused.
func TestProfileMessageWithoutASubagentSectionUsesTheDefaultRootCap(t *testing.T) {
	for _, tc := range []struct {
		below int
		want  string
	}{{15, `"state":"done"`}, {16, "profile_limit_reached"}} {
		p := newPMFixture(t, nil)
		root := seedTree(t, p.f.service.resolver, "other\x00session", 0, tc.below)
		out := p.sendAs(t, p.callerCtx(t, "dev/coder"), tools.ExecutionContext{SessionKey: p.owner, RunNode: root, ChainDepth: 1}, "alpha-1")
		if strings.Contains(out, "pm_") {
			out = p.wait(t, p.handle(t, out))
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%d below the root: want %s:\n%s", tc.below, tc.want, out)
		}
	}
}
