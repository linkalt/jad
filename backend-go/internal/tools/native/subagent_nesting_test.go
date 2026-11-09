package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type nestedBehaviour func(context.Context, SubagentRequest) (SubagentResult, error)

func nestingAgent(name string, names ...string) sessionfs.NativeSubagentAgent {
	agent := agentNamed(name)
	agent.Tools, agent.HasTools = names, true
	return agent
}

func nestingSettings(agents ...sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentSettings {
	settings := subagentSettings(agents...)
	settings.MaxConcurrent = 8
	settings.MaxDepth = 2
	settings.MaxRunsPerRoot = 16
	settings.MaxRunsPerSession = 32
	return settings
}

// nestingFixture builds a subagent tool whose launcher runs the behaviour named after the
// agent, over a parent that has read and async.
func nestingFixture(t *testing.T, settings sessionfs.NativeSubagentSettings, behaviours map[string]nestedBehaviour) (*tools.Catalog, *SubagentManager, *AsyncHost) {
	t.Helper()
	host := newTestAsyncHost(t)
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	parent := append(withExecutors(t, readOnlyTool("read")), asyncTool)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		return behaviours[request.Agent.Name](ctx, request)
	}}
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, manager, host
}

// childCall calls a tool of a child's own catalog the way the child's runner does.
func childCall(t *testing.T, ctx context.Context, request SubagentRequest, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "n1", "name": name, "arguments": arguments})
	raw, err := request.Tools.ExecuteFunctionCall(ctx, call, request.Execution)
	if err != nil {
		t.Errorf("child call %s: %v", name, err)
		return ""
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Errorf("child call %s: %v", name, err)
	}
	return item.Output
}

func holdUntilCancelled(cancelled *atomic.Int64) nestedBehaviour {
	return func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		if cancelled != nil {
			cancelled.Add(1)
		}
		return SubagentResult{}, ctx.Err()
	}
}

func TestNestedStartIsRefusedBeyondMaxDepth(t *testing.T) {
	var midSaw, midGate, bossSaw string
	var owner *tools.Catalog
	catalog, _, _ := nestingFixture(t, nestingSettings(nestingAgent("boss", "read", "subagent"), nestingAgent("mid", "read", "subagent"), nestingAgent("leaf", "read")),
		map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				bossSaw = childCall(t, ctx, request, "subagent", `{"task":"go on","agent":"mid"}`)
				return SubagentResult{Text: "boss done"}, nil
			},
			"mid": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				midSaw = childCall(t, ctx, request, "subagent", `{"task":"go on","agent":"leaf"}`)
				// The tool is hidden at the deepest level; the depth check is the gate behind it.
				call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "gate", "name": "subagent", "arguments": `{"task":"go on","agent":"leaf"}`})
				raw, err := owner.ExecuteFunctionCall(ctx, call, request.Execution)
				if err != nil {
					t.Errorf("owner call: %v", err)
				}
				midGate = string(raw)
				return SubagentResult{Text: "mid done"}, nil
			},
			"leaf": func(context.Context, SubagentRequest) (SubagentResult, error) {
				return SubagentResult{Text: "leaf"}, nil
			},
		})
	owner = catalog
	if out := subagentCall(t, catalog, `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
		t.Fatalf("boss: %s", out)
	}
	if !strings.Contains(bossSaw, "Agent mid finished") {
		t.Errorf("a child at depth 1 could not start a child at depth 2: %s", bossSaw)
	}
	if strings.Contains(midSaw, "Agent leaf finished") || !strings.Contains(midSaw, "unknown_tool") {
		t.Errorf("a child at depth 2 was offered the subagent tool or started a third level: %s", midSaw)
	}
	if !strings.Contains(midGate, "subagent_depth_exceeded") {
		t.Errorf("the depth check did not refuse a start at depth 2: %s", midGate)
	}
}

func TestNestedStartsAreBoundedPerRootAndPerSessionAndNeverQueue(t *testing.T) {
	for name, tc := range map[string]struct {
		tune     func(*sessionfs.NativeSubagentSettings)
		wantOK   int
		wantCode string
	}{
		"per root":     {func(s *sessionfs.NativeSubagentSettings) { s.MaxRunsPerRoot = 2 }, 2, "subagent_limit_reached"},
		"per session":  {func(s *sessionfs.NativeSubagentSettings) { s.MaxRunsPerSession = 3 }, 2, "subagent_limit_reached"},
		"no slot left": {func(s *sessionfs.NativeSubagentSettings) { s.MaxConcurrent = 2; s.MaxQueued = 4 }, 1, "subagent_busy"},
	} {
		t.Run(name, func(t *testing.T) {
			var outputs []string
			settings := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
			tc.tune(&settings)
			catalog, manager, _ := nestingFixture(t, settings, map[string]nestedBehaviour{
				"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
					for range 3 {
						outputs = append(outputs, childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf","block_ms":0}`))
					}
					return SubagentResult{Text: "started"}, nil
				},
				"leaf": holdUntilCancelled(nil),
			})
			if out := subagentCall(t, catalog, `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
				t.Fatalf("boss: %s", out)
			}
			started, refused := 0, ""
			for _, out := range outputs {
				if strings.Contains(out, "is still running") {
					started++
				} else if refused == "" {
					refused = out
				}
			}
			if started != tc.wantOK || !strings.Contains(refused, tc.wantCode) {
				t.Fatalf("started %d (want %d), first refusal %q (want %s); outputs:\n%s", started, tc.wantOK, refused, tc.wantCode, strings.Join(outputs, "\n--\n"))
			}
			manager.mu.Lock()
			queued := manager.queued
			manager.mu.Unlock()
			if queued != 0 {
				t.Errorf("a nested start was queued: %d waiting", queued)
			}
		})
	}
}

func TestStoppingARunCancelsItsDescendantsAndLeavesNoGoroutines(t *testing.T) {
	baseline := baselineCount()
	var cancelled atomic.Int64
	leafStarted := make(chan string, 1)
	catalog, manager, host := nestingFixture(t, nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("mid", "read", "subagent", "async"), nestingAgent("leaf", "read")),
		map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				childCall(t, ctx, request, "subagent", `{"task":"t","agent":"mid","block_ms":0}`)
				<-ctx.Done()
				cancelled.Add(1)
				return SubagentResult{}, ctx.Err()
			},
			"mid": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				leafStarted <- childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf","block_ms":0}`)
				<-ctx.Done()
				cancelled.Add(1)
				return SubagentResult{}, ctx.Err()
			},
			"leaf": holdUntilCancelled(&cancelled),
		})
	bossID := runIDOf(t, subagentCall(t, catalog, `{"task":"x","agent":"boss","block_ms":0}`))
	select {
	case out := <-leafStarted:
		if !strings.Contains(out, "unknown_tool") && !strings.Contains(out, "depth_exceeded") && !strings.Contains(out, "is still running") {
			t.Fatalf("unexpected result of the third level: %s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second level never started")
	}
	asyncCall(t, catalog, `{"action":"stop","id":"`+bossID+`"}`)
	deadline := time.Now().Add(5 * time.Second)
	for cancelled.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := cancelled.Load(); got != 2 {
		t.Fatalf("%d of 2 runs (child and grandchild) saw their context end", got)
	}
	_ = manager.Close()
	host.Close()
	requireNoGrowth(t, baseline, "a stopped run tree")
}

func TestAChildSeesOnlyTheHandlesItsOwnRunStarted(t *testing.T) {
	leafIDs := make(chan string, 1)
	siblingSaw := make(chan string, 1)
	var bossOne atomic.Value
	release := make(chan struct{})
	settings := nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read"))
	settings.BackgroundWriters, settings.MaxBackgroundWriters = true, 4
	catalog, _, _ := nestingFixture(t, settings,
		map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				if request.Task == "one" {
					bossOne.Store(request.Execution.ProcessOwner)
					leafIDs <- runIDOf(t, childCall(t, ctx, request, "subagent", `{"task":"t","agent":"leaf","block_ms":0}`))
					<-release
					return SubagentResult{Text: "one done"}, nil
				}
				leaf := <-leafIDs
				leafIDs <- leaf
				siblingSaw <- childCall(t, ctx, request, "async", `{"action":"status","id":"`+leaf+`"}`)
				return SubagentResult{Text: "two done"}, nil
			},
			"leaf": holdUntilCancelled(nil),
		})
	runIDOf(t, subagentCall(t, catalog, `{"task":"one","agent":"boss","block_ms":0}`))
	subagentCall(t, catalog, `{"task":"two","agent":"boss"}`)
	leaf := <-leafIDs
	if got := <-siblingSaw; !strings.Contains(got, "unknown_handle") {
		t.Errorf("a sibling run saw the first child's handle: %s", got)
	}
	if all := asyncCall(t, catalog, `{"action":"list","state":"all"}`); strings.Contains(all, leaf) {
		t.Errorf("the session's async list shows a grandchild's handle:\n%s", all)
	}
	close(release)
}

func TestANestedToolRunsAtOnceInAChild(t *testing.T) {
	var saw string
	host := newTestAsyncHost(t)
	smith := withExecutors(t, tools.Tool{Definition: tools.Definition{ID: toolSmithToolID, Source: "native", Name: "tool_smith", Description: "x"}, ReadOnly: true})
	parent := append(withExecutors(t, readOnlyTool("read")), smith...)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		saw = childCall(t, ctx, request, "tool_smith", `{}`)
		return SubagentResult{Text: "done"}, nil
	}}
	tool, err := newSubagentToolAsync(nestingSettings(nestingAgent("scripter", "read", "tool_smith")), parent, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if strings.Contains(saw, "approval") || saw == "" {
		t.Errorf("a nested tool was gated or never called: %q", saw)
	}
}

// A nested run belongs to the session that started its tree: the session's listing, its
// cancel and the transcript gate see it, while its handle stays the child's.
func TestNestedRunBelongsToTheSessionThatStartedTheTree(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	grandchild := make(chan string, 1)
	catalog, manager, _ := nestingFixture(t, nestingSettings(nestingAgent("boss", "read", "subagent", "async"), nestingAgent("leaf", "read")),
		map[string]nestedBehaviour{
			"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
				grandchild <- childCall(t, ctx, request, "subagent", `{"task":"deep","agent":"leaf","block_ms":5}`)
				<-release
				return SubagentResult{Text: "boss done"}, nil
			},
			"leaf": blockedLauncher(release, nil).run,
		})
	go func() {
		_, _ = catalog.ExecuteFunctionCall(context.Background(), []byte(`{"type":"function_call","call_id":"c","name":"subagent","arguments":"{\"task\":\"go\",\"agent\":\"boss\",\"block_ms\":5}"}`), tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: "alpha"})
	}()
	var childID string
	select {
	case out := <-grandchild:
		childID = runIDOf(t, out)
	case <-time.After(5 * time.Second):
		t.Fatal("the boss did not start its grandchild")
	}
	if _, owner, found := manager.RunTranscript(childID); !found || owner != "alpha" {
		t.Fatalf("RunTranscript(grandchild) owner = %q, found %v; want the session alpha", owner, found)
	}
	if got := len(manager.list("alpha")); got != 2 {
		t.Errorf("list(alpha) = %d runs, want 2 (the child and its grandchild)", got)
	}
	if got := manager.CancelOwner("alpha"); got != 2 {
		t.Errorf("CancelOwner(alpha) = %d, want 2: the grandchild is the session's run too", got)
	}
}
