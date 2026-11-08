package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// delegatingTool is a runner tool like load_tool: ReadOnly, and a call with run delegates to the
// tool run names, with arguments as its arguments.
func delegatingTool(t *testing.T) Tool {
	t.Helper()
	runner := lazyTool(t, "native.runner", "native", "runner")
	runner.ReadOnly = true
	runner.Definition.InputSchema = json.RawMessage(`{"type":"object"}`)
	runner.Delegate = func(arguments map[string]any) (string, map[string]any, bool) {
		target, _ := arguments["run"].(string)
		if target == "" {
			return "", nil, false
		}
		targetArguments, _ := arguments["arguments"].(map[string]any)
		if targetArguments == nil {
			targetArguments = map[string]any{}
		}
		return target, targetArguments, true
	}
	return runner
}

func delegateCatalog(t *testing.T) *Catalog {
	t.Helper()
	write := lazyTool(t, "native.write", "native", "write")
	read := lazyTool(t, "native.read", "native", "read")
	read.ReadOnly = true
	leaf := lazyTool(t, "native.lsp", "native", "lsp")
	leaf.ReadOnly = true
	leaf.LeafBudget = true
	guarded := lazyTool(t, "native.guarded", "native", "guarded")
	guarded.ReadOnly = true
	guarded.PlanGuarded = true
	decide := lazyTool(t, "native.decide", "native", "decide")
	decide.ReadOnly = true
	decide.RequiresUserDecision = true
	maybe := lazyTool(t, "native.maybe", "native", "maybe")
	maybe.ReadOnly = true
	maybe.MayChangeFiles = func(arguments map[string]any) bool { return arguments["write"] == true }
	remote := lazyTool(t, "mcp:docs:fetch", "mcp", "mcp_docs_fetch_0123456789")
	catalog, err := NewCatalog([]Tool{delegatingTool(t), write, read, leaf, guarded, decide, maybe, remote})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func runnerCall(name, arguments string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c1", "name": name, "arguments": arguments})
	return encoded
}

func TestResolveCallRewritesADelegatingCallToItsTarget(t *testing.T) {
	catalog := delegateCatalog(t)
	resolved := catalog.ResolveCall(runnerCall("runner", `{"run":"write","arguments":{"path":"a","n":12345678901234567890}}`))
	var fields map[string]string
	if err := json.Unmarshal(resolved, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["name"] != "write" || fields["call_id"] != "c1" || fields["type"] != "function_call" {
		t.Fatalf("resolved = %s, want the same call_id with the target's name", resolved)
	}
	if fields["arguments"] != `{"n":12345678901234567890,"path":"a"}` {
		t.Fatalf("resolved arguments = %s, want the target's arguments with the number kept exact", fields["arguments"])
	}
	// The ID names the tool too.
	var byID map[string]string
	_ = json.Unmarshal(catalog.ResolveCall(runnerCall("runner", `{"run":"mcp:docs:fetch"}`)), &byID)
	if byID["name"] != "mcp_docs_fetch_0123456789" || byID["arguments"] != "{}" {
		t.Fatalf("a run by ID resolved to %v, want the model-facing name and {} arguments", byID)
	}
}

func TestResolveCallLeavesEveryOtherCallAlone(t *testing.T) {
	catalog := delegateCatalog(t)
	for name, call := range map[string]json.RawMessage{
		"a list":            runnerCall("runner", `{}`),
		"an unknown target": runnerCall("runner", `{"run":"nothing"}`),
		"a runner target":   runnerCall("runner", `{"run":"runner","arguments":{"run":"write"}}`),
		"a plain tool":      runnerCall("write", `{"run":"read"}`),
		"unknown tool":      runnerCall("nothing", `{"run":"read"}`),
		"bad arguments":     runnerCall("runner", `{"run":`),
		"array arguments":   runnerCall("runner", `[1]`),
		"not a call":        json.RawMessage(`[]`),
	} {
		if got := catalog.ResolveCall(call); string(got) != string(call) {
			t.Errorf("%s: resolved to %s, want the call unchanged", name, got)
		}
	}
	var nilCatalog *Catalog
	call := runnerCall("runner", `{"run":"write"}`)
	if got := nilCatalog.ResolveCall(call); string(got) != string(call) {
		t.Errorf("a nil catalog changed the call: %s", got)
	}
}

// Every judgment the session makes about a call is made about the tool a runner runs, not about the
// runner: a run of a write is a write, a run of a leaf is a leaf, and so on.
func TestJudgmentsAskAboutTheTargetOfARunnerCall(t *testing.T) {
	catalog := delegateCatalog(t)
	run := func(target, arguments string) json.RawMessage {
		encoded, _ := json.Marshal(map[string]any{"run": target, "arguments": json.RawMessage(arguments)})
		return runnerCall("runner", string(encoded))
	}
	if !catalog.ChangesState(run("write", `{}`)) || catalog.ChangesState(run("read", `{}`)) || catalog.ChangesState(runnerCall("runner", `{}`)) {
		t.Error("ChangesState must follow the target: write changes state, read and a list do not")
	}
	if !catalog.ChangesState(run("maybe", `{"write":true}`)) || catalog.ChangesState(run("maybe", `{"write":false}`)) {
		t.Error("ChangesState must apply the target's per-call MayChangeFiles to the target's arguments")
	}
	if !catalog.CallMayChangeFiles(run("maybe", `{"write":true}`)) || catalog.CallMayChangeFiles(run("maybe", `{}`)) {
		t.Error("CallMayChangeFiles must follow the target's arguments")
	}
	if !catalog.IsLeafCall(run("lsp", `{}`)) || catalog.IsLeafCall(run("read", `{}`)) || catalog.IsLeafCall(runnerCall("runner", `{}`)) {
		t.Error("IsLeafCall must follow the target")
	}
	if !catalog.IsPlanGuardedCall(run("guarded", `{}`)) || catalog.IsPlanGuardedCall(run("read", `{}`)) {
		t.Error("IsPlanGuardedCall must follow the target")
	}
	if !catalog.IsUserDecisionCall(run("decide", `{}`)) || catalog.IsUserDecisionCall(run("read", `{}`)) {
		t.Error("IsUserDecisionCall must follow the target")
	}
	if got := catalog.SourceOf(run("mcp:docs:fetch", `{}`)); got != "mcp" {
		t.Errorf("SourceOf(run mcp) = %q, want mcp", got)
	}
	if got := catalog.SourceOf(runnerCall("runner", `{}`)); got != "native" {
		t.Errorf("SourceOf(list) = %q, want the runner's own native", got)
	}
}

func TestLeafBudgetIsNeverSetOnARunner(t *testing.T) {
	if delegatingTool(t).LeafBudget {
		t.Fatal("a runner must not hold a leaf token itself: it waits on the call it dispatches")
	}
}

// The catalog tells each call which catalog dispatches it, and ForNested drops it with the rest of
// the per-call identity.
func TestCatalogSetsTheDispatcherOfEveryCall(t *testing.T) {
	var seen ExecutionContext
	probe := lazyTool(t, "native.probe", "native", "probe")
	probe.Execute = func(_ context.Context, _ map[string]any, execution ExecutionContext) (string, error) {
		seen = execution
		return "ok", nil
	}
	catalog, err := NewCatalog([]Tool{probe})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ExecuteFunctionCall(context.Background(), runnerCall("probe", `{}`), ExecutionContext{ViaRunner: false}); err != nil {
		t.Fatal(err)
	}
	if seen.Dispatcher != Dispatcher(catalog) {
		t.Fatalf("Dispatcher = %v, want the executing catalog", seen.Dispatcher)
	}
	if nested := seen.ForNested(); nested.Dispatcher != nil || nested.ViaRunner {
		t.Fatalf("ForNested kept the dispatcher or ViaRunner: %+v", nested)
	}
	if !catalog.Has("probe") || catalog.Has("nothing") {
		t.Fatal("Has must report the catalog's own names")
	}
}
