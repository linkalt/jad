package native

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// Run tree release characterisation (T-007 Q0): every way a local sub-agent run ends, or a start
// fails after admission, gives back every place it took in the run caps, so a later start is
// never refused by a run that is gone. liveTreeRuns is the measuring instrument.

// liveTreeRuns is how many runs the manager's run caps count as going: the nodes of its run tree.
func liveTreeRuns(m *SubagentManager) int {
	return len(m.runTree().Nodes())
}

// seedRunRoot puts a top-level run in m's tree, executed in another session so it counts against no
// session cap, and returns its node: a hand-built nested call carries it as its RunNode.
func seedRunRoot(t testing.TB, m *SubagentManager) string {
	t.Helper()
	node, err := m.runTree().Reserve(RunReservation{Session: admissionSeedSession, Kind: RunNodeSubagent, State: RunNodeRunning})
	if err != nil {
		t.Fatal(err)
	}
	return node.ID()
}

// nestedExecution is execution as a call from inside a run of m's tree when its AgentDepth is above
// 0: it carries a seeded root (RootRun "sub_root") as the calling run's node.
func nestedExecution(t testing.TB, m *SubagentManager, execution tools.ExecutionContext) tools.ExecutionContext {
	t.Helper()
	if execution.AgentDepth > 0 {
		execution.RootRun, execution.RunNode = "sub_root", seedRunRoot(t, m)
	}
	return execution
}

func awaitNoLiveTreeRuns(t *testing.T, m *SubagentManager, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for liveTreeRuns(m) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := liveTreeRuns(m); got != 0 {
		t.Fatalf("%s: the run caps still count %d runs after the run ended", path, got)
	}
}

func releaseFixture(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher) (*tools.Catalog, *SubagentManager) {
	t.Helper()
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	host := newTestAsyncHost(t)
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	return catalogWithAsync(t, host, parent, tool), manager
}

func TestEveryLocalRunEndGivesBackItsRunCapPlaces(t *testing.T) {
	answer := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: "ok"}, nil
	}}
	failing := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{}, errors.New("boom")
	}}
	panicking := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) { panic("boom") }}
	settings := subagentSettings(agentNamed("explore"))

	t.Run("done", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, answer)
		if out := subagentCall(t, catalog, `{"task":"x"}`); !strings.Contains(out, "finished") {
			t.Fatalf("output:\n%s", out)
		}
		awaitNoLiveTreeRuns(t, manager, "done")
	})
	t.Run("failed", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, failing)
		if out := subagentCall(t, catalog, `{"task":"x"}`); !strings.Contains(out, "subagent_failed") {
			t.Fatalf("output:\n%s", out)
		}
		awaitNoLiveTreeRuns(t, manager, "failed")
	})
	t.Run("launcher panic", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, panicking)
		subagentCall(t, catalog, `{"task":"x"}`)
		awaitNoLiveTreeRuns(t, manager, "launcher panic")
	})
	t.Run("panic after the launcher", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, answer)
		manager.afterLaunch = func() { panic("sweep exploded") }
		if _, returned := callReturnsWithin(t, catalog, `{"task":"x"}`, 5*time.Second); !returned {
			t.Fatal("the call never returned")
		}
		awaitNoLiveTreeRuns(t, manager, "panic after the launcher")
	})
	t.Run("stopped while running", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		catalog, manager := releaseFixture(t, settings, blockedLauncher(release, nil))
		id := runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`))
		if liveTreeRuns(manager) != 1 {
			t.Fatalf("a running run counts %d runs, want 1", liveTreeRuns(manager))
		}
		asyncCall(t, catalog, `{"action":"stop","id":"`+id+`"}`)
		awaitNoLiveTreeRuns(t, manager, "stopped while running")
	})
	t.Run("stopped while queued", func(t *testing.T) {
		one := settings
		one.MaxConcurrent, one.MaxQueued = 1, 2
		release := make(chan struct{})
		catalog, manager := releaseFixture(t, one, blockedLauncher(release, nil))
		runIDOf(t, subagentCall(t, catalog, `{"task":"first","block_ms":0}`))
		queued := runIDOf(t, subagentCall(t, catalog, `{"task":"second","block_ms":0}`))
		if liveTreeRuns(manager) != 2 {
			t.Fatalf("a running and a queued run count %d runs, want 2", liveTreeRuns(manager))
		}
		asyncCall(t, catalog, `{"action":"stop","id":"`+queued+`"}`)
		deadline := time.Now().Add(5 * time.Second)
		for liveTreeRuns(manager) != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := liveTreeRuns(manager); got != 1 {
			t.Fatalf("after the queued run was stopped the caps count %d runs, want 1", got)
		}
		close(release)
		awaitNoLiveTreeRuns(t, manager, "stopped while queued")
	})
	t.Run("closed between the handle and the run", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, answer)
		manager.afterBegin = func(string) { _ = manager.Close() }
		if out := subagentCall(t, catalog, `{"task":"x","block_ms":0}`); !strings.Contains(out, "subagent_unavailable") {
			t.Fatalf("output:\n%s", out)
		}
		awaitNoLiveTreeRuns(t, manager, "closed between the handle and the run")
	})
	t.Run("manager closed while running", func(t *testing.T) {
		catalog, manager := releaseFixture(t, settings, blockedLauncher(make(chan struct{}), nil))
		runIDOf(t, subagentCall(t, catalog, `{"task":"x","block_ms":0}`))
		_ = manager.Close()
		awaitNoLiveTreeRuns(t, manager, "manager closed while running")
	})
	t.Run("refused for a full queue", func(t *testing.T) {
		one := settings
		one.MaxConcurrent, one.MaxQueued = 1, 0
		release := make(chan struct{})
		catalog, manager := releaseFixture(t, one, blockedLauncher(release, nil))
		runIDOf(t, subagentCall(t, catalog, `{"task":"first","block_ms":0}`))
		if out := subagentCall(t, catalog, `{"task":"second","block_ms":0}`); !strings.Contains(out, "subagent_busy") {
			t.Fatalf("output:\n%s", out)
		}
		if got := liveTreeRuns(manager); got != 1 {
			t.Fatalf("a start refused for a full queue left a node: %d runs, want 1", got)
		}
		close(release)
		awaitNoLiveTreeRuns(t, manager, "refused for a full queue")
	})
	t.Run("nested start refused for no slot", func(t *testing.T) {
		nested := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
		nested.MaxConcurrent = 1
		var out string
		catalog, manager, _ := nestingFixture(t, nested, map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				out = childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf"}`)
				return SubagentResult{Text: "done"}, nil
			},
		})
		subagentCall(t, catalog, `{"task":"x","agent":"boss"}`)
		if !strings.Contains(out, "subagent_busy") {
			t.Fatalf("the nested start: %s", out)
		}
		awaitNoLiveTreeRuns(t, manager, "nested start refused for no slot")
	})
	t.Run("nested starts one after another", func(t *testing.T) {
		nested := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
		nested.MaxRunsPerRoot = 1
		var outputs []string
		catalog, manager, _ := nestingFixture(t, nested, map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				for range 3 {
					outputs = append(outputs, childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf"}`))
				}
				return SubagentResult{Text: "started"}, nil
			},
			"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
				return SubagentResult{Text: "leaf"}, nil
			},
		})
		if out := subagentCall(t, catalog, `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
			t.Fatalf("boss: %s", out)
		}
		for _, out := range outputs {
			if !strings.Contains(out, "Agent leaf finished") {
				t.Fatalf("a nested start after the earlier one ended was refused; outputs:\n%s", strings.Join(outputs, "\n--\n"))
			}
		}
		awaitNoLiveTreeRuns(t, manager, "nested starts one after another")
	})
}
