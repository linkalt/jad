package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// R6 mixed depth (design §3.5): max_depth and fail fast go by a call's RunDepth, its depth across
// both kinds of run. A profile_message target runs with ChainDepth = its caller's RunDepth + 1 and
// AgentDepth 0: it is a session's own run, so resume, compact and reset, the transcript default
// and the background writer place still see depth 0. The run caps go by its run tree node (T-007).

// chainCall runs one subagent call with execution and returns its output.
func chainCall(t *testing.T, catalog *tools.Catalog, execution tools.ExecutionContext, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_a", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func chainExecution(chainDepth int) tools.ExecutionContext {
	return tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, ChainDepth: chainDepth}
}

// A child gets AgentDepth 1 and ChainDepth one below its caller's RunDepth, is the root of a tree of
// its own, and is at the deepest level (no subagent tool) when ChainDepth+1 reaches max_depth.
func TestAChildIsOneBelowItsCallersChainDepth(t *testing.T) {
	for _, tc := range []struct {
		chainDepth, wantChain int
		wantNests             bool
	}{
		{0, 1, true},
		{1, 2, false},
	} {
		var saw tools.ExecutionContext
		var nested string
		catalog, _, _ := nestingFixture(t, nestingSettings(nestingAgent("boss", "read", "subagent"), nestingAgent("leaf", "read")), map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				saw = request.Execution
				nested = childCall(t, ctx, request, "subagent", `{"task":"go on","agent":"leaf"}`)
				return SubagentResult{Text: "boss done"}, nil
			},
			"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
				return SubagentResult{Text: "leaf"}, nil
			},
		})
		if out := chainCall(t, catalog, chainExecution(tc.chainDepth), `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
			t.Fatalf("chain depth %d: %s", tc.chainDepth, out)
		}
		if saw.AgentDepth != 1 || saw.ChainDepth != tc.wantChain || saw.RootRun == "" || saw.RootRun != saw.ProcessOwner {
			t.Errorf("chain depth %d: child AgentDepth %d ChainDepth %d RootRun %q owner %q; want 1, %d and its own id", tc.chainDepth, saw.AgentDepth, saw.ChainDepth, saw.RootRun, saw.ProcessOwner, tc.wantChain)
		}
		if nests := strings.Contains(nested, "Agent leaf finished"); nests != tc.wantNests || (!nests && !strings.Contains(nested, "unknown_tool")) {
			t.Errorf("chain depth %d: the child's own subagent call = %s, want nesting %v", tc.chainDepth, nested, tc.wantNests)
		}
	}
}

// A start from a call with ChainDepth above 0 but no run tree node (AgentDepth 0) passes the depth
// gate on its chain depth and fails fast without a slot, but the run caps do not count it: it is a
// root. A real profile_message target carries its node since T-007 and is capped
// (TestAStartFromAProfileTargetIsCappedInItsCallersTree).
func TestAStartWithNoRunNodeIsGatedAndFailsFastButNotCapped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chainDepth int
		seed       admissionSeed
		want       string
	}{
		{"at max_depth", 2, admissionSeed{}, "subagent_depth_exceeded"},
		{"no slot, queue room", 1, admissionSeed{running: 1}, "subagent_busy"},
		{"no slot and caps hit", 1, admissionSeed{root: 3, session: 3, running: 1}, "subagent_busy"},
		{"caps hit, a free slot", 1, admissionSeed{root: 3, session: 3}, "admitted: holds a slot"},
	} {
		var launched atomic.Int32
		catalog, manager, _ := nestingFixture(t, admissionSettings(), map[string]nestedBehaviour{
			"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
				launched.Add(1)
				return SubagentResult{Text: "leaf"}, nil
			},
		})
		tc.seed.apply(manager, testSessionKey)
		out := admissionOutcome(manager, tc.seed, chainCall(t, catalog, chainExecution(tc.chainDepth), `{"task":"t","agent":"leaf","block_ms":0}`))
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: %s, want %s", tc.name, out, tc.want)
		}
		manager.mu.Lock()
		queued := manager.queued
		manager.mu.Unlock()
		if queued != 0 {
			t.Errorf("%s: the start was queued", tc.name)
		}
		if tc.want != "admitted: holds a slot" && launched.Load() != 0 {
			t.Errorf("%s: a refused start ran", tc.name)
		}
		_ = manager.Close()
	}
}

// A remote start from a profile_message target is gated on the chain depth the same way and, not
// being nested, takes no place in the run caps.
func TestARemoteStartFromAProfileTargetIsGatedOnItsChainDepth(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, func(s *sessionfs.NativeSubagentSettings) {
		s.MaxDepth, s.MaxRunsPerRoot, s.MaxRunsPerSession = 2, 2, 3
	})
	atMax := remoteCaller
	atMax.ChainDepth = 2
	if out := f.start(t, remoteAgent, 0, atMax); !strings.Contains(out, "subagent_depth_exceeded") || len(f.peer.backend.subs) != 0 {
		t.Fatalf("a remote start at max_depth by chain depth reached the peer:\n%s", out)
	}
	admissionSeed{session: 3}.apply(f.manager, remoteCaller.SessionKey)
	var counted atomic.Int64
	counted.Store(-1)
	backend := f.peer.backend
	backend.onSubagentStart = func() {
		counted.Store(int64(f.manager.sessionRuns(remoteCaller.SessionKey)))
		backend.runs[len(backend.runs)-1].finish(remoteapi.RunResult{State: "done", Text: "ok"})
	}
	below := remoteCaller
	below.ChainDepth = 1
	if out := f.start(t, remoteAgent, 5000, below); !strings.Contains(out, "finished") || counted.Load() != 3 {
		t.Fatalf("a remote start below max_depth (session counted %d, seeded 3):\n%s", counted.Load(), out)
	}
}

// A profile_message target deep in a chain is still a session's own run: it may compact and reset
// a profile and resume a run of its session, which a sub-agent may not.
func TestAProfileTargetDeepInAChainKeepsItsOwnRunActions(t *testing.T) {
	actions := &fakeProfileActions{}
	tool := newProfileActionsTool(t, profileTestSettings(), actions)
	for _, action := range []string{"compact", "reset"} {
		if out := callTranscripts(t, tool, `{"action":"`+action+`","path":"profile:dev/reviewer"}`, tools.ExecutionContext{SessionKey: listCaller, ChainDepth: 3}); strings.Contains(out, "profile_children_denied") {
			t.Fatalf("%s from a profile target at chain depth 3 = %s", action, out)
		}
	}
	if len(actions.calls) != 2 {
		t.Fatalf("calls = %v, want compact and reset", actions.calls)
	}

	root := t.TempDir()
	host := newTestAsyncHost(t)
	settings := subagentSettings(agentNamed("explore"))
	settings.StorageDir, settings.PersistRuns, settings.KeepFinished, settings.MaxDepth = root, true, 10, 2
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	subagent, err := newSubagentToolAsync(settings, parent, nil, transcribingLauncher(t, root), nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog := catalogWithAsync(t, host, parent, subagent)
	source := finishedSource(t, catalog, manager, testSessionKey)
	execution := chainExecution(1)
	if out := chainCall(t, catalog, execution, `{"resume":"`+source+`","task":"more","block_ms":0}`); !strings.Contains(out, "resumed_from: "+source) {
		t.Fatalf("a profile target at chain depth 1 could not resume a run of its session:\n%s", out)
	}
}

func TestRunDepthIsTheDeeperOfChainAndAgentDepth(t *testing.T) {
	for _, tc := range []struct{ chain, agent, want int }{{0, 0, 0}, {2, 0, 2}, {0, 2, 2}, {3, 1, 3}} {
		execution := tools.ExecutionContext{ChainDepth: tc.chain, AgentDepth: tc.agent}
		if got := execution.RunDepth(); got != tc.want {
			t.Errorf("ExecutionContext{ChainDepth %d, AgentDepth %d}.RunDepth() = %d, want %d", tc.chain, tc.agent, got, tc.want)
		}
		if got := execution.Caller().RunDepth(); got != tc.want {
			t.Errorf("CallerContext of ChainDepth %d, AgentDepth %d: RunDepth() = %d, want %d", tc.chain, tc.agent, got, tc.want)
		}
	}
}
