package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// denyFixture is a session whose subagent tool runs the behaviour named after the agent, over a
// parent with read, grep, async and tool_smith; tool_smith's scripts may call read and grep.
// grepRan counts the grep runs that reached the tool, from the model or from a script.
type denyFixture struct {
	catalog *tools.Catalog
	root    string
	grepRan *atomic.Int64
}

func newDenyFixture(t *testing.T, settings sessionfs.NativeSubagentSettings, behaviours map[string]nestedBehaviour) denyFixture {
	t.Helper()
	root := t.TempDir()
	shortTempDir(t)
	grepRan := &atomic.Int64{}
	read := bridgeTestTool(t, "read", true, func(map[string]any) string { return "read ran" })
	grep := bridgeTestTool(t, "grep", true, func(map[string]any) string { grepRan.Add(1); return "grep ran" })
	nested := &ToolSmithNested{}
	smith, err := NewToolSmithTool(bridgeSettings(t, root, "read", "grep"), "", nested, nil)
	if err != nil {
		t.Fatal(err)
	}
	host := newTestAsyncHost(t)
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	parent := []tools.Tool{read, grep, asyncTool, smith}
	nested.Bind(parent, sessionfs.ProfileToolSettings{Restricted: true, Deny: settings.DenyTools})
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
	return denyFixture{catalog: catalog, root: root, grepRan: grepRan}
}

// call runs one call of the session's own run, whose tools.deny entries are deny.
func (f denyFixture) call(t *testing.T, name, arguments string, deny ...string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_1", "name": name, "arguments": arguments})
	raw, err := f.catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: f.root, SessionDir: f.root, SessionKey: testSessionKey, Deny: deny})
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

const grepScript = `{"code":"const r = await tools.grep({}); return r.status + ':' + r.ok;"}`

// A script of a child cannot reach a tool its agent denies (U3, the reported tool_smith bypass):
// the bridge of the owner's tool_smith applies the deny of the run that calls it, not only the
// owner's profile. Before U3 the script ran grep although the child's own catalog had no grep.
func TestToolSmithScriptOfAChildCannotRunAToolItsAgentDenies(t *testing.T) {
	toolSmithInterpreter(t)
	scripter := agentNamed("scripter")
	scripter.DenyTools = []string{"grep"}
	var childTools, script string
	f := newDenyFixture(t, nestingSettings(scripter), map[string]nestedBehaviour{
		"scripter": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			childTools = strings.Join(toolNames(request.Tools), ",")
			script = childCall(t, ctx, request, "tool_smith", grepScript)
			return SubagentResult{Text: "done"}, nil
		},
	})
	if out := f.call(t, "subagent", `{"task":"x","agent":"scripter"}`); !strings.Contains(out, "Agent scripter finished") {
		t.Fatalf("subagent: %s", out)
	}
	if strings.Contains(","+childTools+",", ",grep,") {
		t.Fatalf("the child's catalog holds the denied grep: %s", childTools)
	}
	if f.grepRan.Load() != 0 || !strings.Contains(script, "invalid_input:false") {
		t.Fatalf("a script of the child ran grep %d time(s), which its agent denies:\n%s", f.grepRan.Load(), script)
	}
}

// subagentCallDenied calls the subagent tool of catalog from a run whose tools.deny ceiling is deny.
func subagentCallDenied(t *testing.T, catalog *tools.Catalog, arguments string, deny ...string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey, Deny: deny})
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

// The caller's deny narrows the child of that call only (U3): its catalog loses the denied tools,
// it hands the ceiling on with its agent's deny, and a denied async tool is not its handle mode
// either. A later call without the deny gets the agent's whole catalog, the very one it had.
func TestSubagentChildIsNarrowedByTheCallersDenyForThatCallOnly(t *testing.T) {
	launcher := &fakeLauncher{}
	plain := agentNamed("plain")
	plain.DenyTools = []string{"glob"}
	host := newTestAsyncHost(t)
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	parent := append(withExecutors(t, readOnlyTool("read"), readOnlyTool("grep"), readOnlyTool("glob"), writeTool("write")), asyncTool)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(withDeny(subagentSettings(plain), "write"), parent, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}

	subagentCallDenied(t, catalog, `{"task":"x"}`)
	whole := launcher.requests[0]
	if got := strings.Join(toolNames(whole.Tools), ","); got != "read,grep,async" {
		t.Fatalf("unnarrowed child tools = %q", got)
	}
	if got := strings.Join(whole.Execution.Deny, ","); got != "write,glob" {
		t.Fatalf("unnarrowed child ceiling = %q, want the session's and the agent's deny", got)
	}

	subagentCallDenied(t, catalog, `{"task":"x"}`, "grep", "async", "mcp:other:*")
	narrowed := launcher.requests[1]
	if got := strings.Join(toolNames(narrowed.Tools), ","); got != "read" {
		t.Fatalf("narrowed child tools = %q, want read", got)
	}
	if got := strings.Join(narrowed.Execution.Deny, ","); got != "grep,async,mcp:other:*,write,glob" {
		t.Fatalf("narrowed child ceiling = %q", got)
	}
	if narrowed.Execution.ProcessAsync {
		t.Fatal("a child denied the async tool was given async handles")
	}

	subagentCallDenied(t, catalog, `{"task":"x"}`)
	again := launcher.requests[2]
	if again.Tools != whole.Tools || again.Execution.Deny == nil || !whole.Execution.ProcessAsync {
		t.Fatalf("a later unnarrowed call did not get the agent's own catalog back (same %v, async %v)", again.Tools == whole.Tools, whole.Execution.ProcessAsync)
	}
	// A deny that covers none of the child's tools keeps the very catalog too.
	subagentCallDenied(t, catalog, `{"task":"x"}`, "mcp:other:*", "write")
	if launcher.requests[3].Tools != whole.Tools {
		t.Fatal("a deny that covers nothing gave the child a new catalog")
	}
	// The fingerprint is the agent definition's: a caller's ceiling is inherited, so it does not
	// count as a changed definition on resume.
	manager.mu.Lock()
	configs := map[string]bool{}
	for _, run := range manager.runs {
		configs[run.config] = true
	}
	manager.mu.Unlock()
	if len(configs) != 1 {
		t.Fatalf("the runs have %d fingerprints, want one whatever the caller's deny", len(configs))
	}
}

// A deny that covers every tool of the child leaves it text-only: it still runs.
func TestSubagentChildDeniedEveryToolRunsTextOnly(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentWithTools("reader", "read")), launcher, readOnlyTool("read"))
	if out := subagentCallDenied(t, catalog, `{"task":"x"}`, "read"); !strings.Contains(out, "finished") {
		t.Fatalf("subagent: %s", out)
	}
	if got := toolNames(launcher.requests[0].Tools); len(got) != 0 {
		t.Fatalf("text-only child tools = %v", got)
	}
}

// Each intermediate agent's deny reaches the runs below it (U3): boss denies grep, so its child
// leaf has no grep, although leaf started by the session itself has it.
func TestIntermediateAgentDenyNarrowsItsChildren(t *testing.T) {
	boss := agentNamed("boss")
	boss.DenyTools = []string{"grep"}
	var leafTools []string
	var leafDeny []string
	catalog, _, _ := nestingFixture(t, nestingSettings(boss, agentNamed("leaf")), map[string]nestedBehaviour{
		"boss": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			if out := childCall(t, ctx, request, "subagent", `{"task":"y","agent":"leaf"}`); !strings.Contains(out, "Agent leaf finished") {
				t.Errorf("boss's child: %s", out)
			}
			return SubagentResult{Text: "boss done"}, nil
		},
		"leaf": func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
			leafTools, leafDeny = toolNames(request.Tools), request.Execution.Deny
			return SubagentResult{Text: "leaf"}, nil
		},
	})
	if out := subagentCall(t, catalog, `{"task":"x","agent":"boss"}`); !strings.Contains(out, "Agent boss finished") {
		t.Fatalf("boss: %s", out)
	}
	if strings.Contains(strings.Join(leafTools, ","), "grep") || strings.Join(leafDeny, ",") != "grep" {
		t.Fatalf("boss's leaf: tools %v, ceiling %v; want no grep and boss's deny", leafTools, leafDeny)
	}
	if out := subagentCall(t, catalog, `{"task":"x","agent":"leaf"}`); !strings.Contains(out, "Agent leaf finished") {
		t.Fatalf("leaf: %s", out)
	}
	if !strings.Contains(strings.Join(leafTools, ","), "read") || len(leafDeny) != 0 {
		t.Fatalf("the session's own leaf: tools %v, ceiling %v; want it whole", leafTools, leafDeny)
	}
}

// A toolset child with a lazy MCP tool: a caller's deny of the server takes the tool out of the
// child's catalog, so its load_tool neither describes nor runs it and the server is never called.
// A deny of something else keeps the child's own copy, which still fires the toolset's hooks only.
func TestDenyCeilingNarrowsAToolsetChildsLazyMCPToolAndKeepsItsHooks(t *testing.T) {
	var ownerCalls, toolsetCalls int
	ownerTool := sameIDStub(t, "owner description", "ownerarg", &ownerCalls)
	toolsetTool := sameIDStub(t, "toolset description", "toolsetarg", &toolsetCalls)
	dir := t.TempDir()
	ownerMarker, toolsetMarker := filepath.Join(dir, "owner"), filepath.Join(dir, "toolset")
	ownerHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("owner", "*", "echo fired >> "+ownerMarker)}, nil)
	toolsetHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("toolset", "*", "echo fired >> "+toolsetMarker)}, nil)
	set := tools.NewLazySet([]string{"mcp:docs:search"}, 256, nil)
	set.Bind([]tools.Tool{ownerTool})
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	mcp := NewSubagentMCP(ownerHooks, "")
	launcher := &fakeLauncher{}
	parentTools := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentTool(subagentSettings(toolsetAgent("finder", "grep", "load_tool", "mcp:docs:search")), parentTools,
		map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, launcher, mcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.SetLazy(set, &loadTool); err != nil {
		t.Fatal(err)
	}
	if err := mcp.BindToolsets([]tools.Tool{ownerTool}, map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{toolsetTool}, Hooks: toolsetHooks}}); err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}

	subagentCallDenied(t, catalog, `{"task":"x","agent":"finder"}`, "mcp:docs:*")
	denied := launcher.requests[0].Tools
	if got := strings.Join(toolNames(denied), ","); got != "grep,load_tool" {
		t.Fatalf("narrowed toolset child tools = %q, want grep,load_tool", got)
	}
	described := executeNamedCall(t, denied, "load_tool", `{"names":["mcp:docs:search"]}`)
	ran := executeThroughLoadTool(t, denied, "mcp:docs:search", `{}`)
	if strings.Contains(described, "description") || strings.Contains(ran, "description ran") || ownerCalls+toolsetCalls != 0 {
		t.Fatalf("a denied lazy tool was reached: describe %s; run %s; calls owner %d toolset %d", described, ran, ownerCalls, toolsetCalls)
	}

	subagentCallDenied(t, catalog, `{"task":"x","agent":"finder"}`, "grep")
	kept := launcher.requests[1].Tools
	if got := strings.Join(toolNames(kept), ","); got != "load_tool,mcp_docs_search_0123456789" {
		t.Fatalf("toolset child without grep = %q", got)
	}
	if ran := executeThroughLoadTool(t, kept, "mcp:docs:search", `{}`); !strings.Contains(ran, "toolset description ran") {
		t.Fatalf("run = %s, want the toolset copy's output", ran)
	}
	if ownerCalls != 0 || toolsetCalls != 1 || countLines(t, toolsetMarker) != 1 || countLines(t, ownerMarker) != 0 {
		t.Fatalf("owner ran %d, toolset ran %d, toolset hook fired %d (want 1), owner hook fired %d (want 0)",
			ownerCalls, toolsetCalls, countLines(t, toolsetMarker), countLines(t, ownerMarker))
	}
}

// A script reaches no tool the run that calls it is denied, from any level (U3): the profile's
// own deny was applied already, and the ceiling a caller handed the run is applied too.
func TestToolSmithScriptHonoursTheRunsDenyCeiling(t *testing.T) {
	root, _ := t.TempDir(), shortTempDir(t)
	var grepRan atomic.Int64
	read := bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })
	grep := bridgeTestTool(t, "grep", true, func(map[string]any) string { grepRan.Add(1); return "g" })
	settings := bridgeSettings(t, root, "read", "grep")
	nested := bridgeNested(read, grep)
	script := `const g = await tools.grep({}); const r = await tools.read({}); return g.status + ':' + r.status;`
	if out := execContent(runWithBridge(context.Background(), root, settings, nested, tools.ExecutionContext{Deny: []string{"grep", "mcp:x:*"}}, script)); !strings.Contains(out, "invalid_input:ok") || grepRan.Load() != 0 {
		t.Fatalf("with grep denied: %s (grep ran %d)", out, grepRan.Load())
	}
	if out := execContent(runWithBridge(context.Background(), root, settings, nested, tools.ExecutionContext{}, script)); !strings.Contains(out, "ok:ok") || grepRan.Load() != 1 {
		t.Fatalf("without a deny: %s (grep ran %d)", out, grepRan.Load())
	}
}

// A script of a child reaches only the tool_smith tools its own agent may use (U3 follow-up): the
// bridge applies the child's own tools (ExecutionContext.RunTools, its catalog), not only the
// owner's profile. scripter lists tool_smith, read and subagent; before the fix its script ran grep.
// Its own allow list is not handed on: the child scripter starts, whose agent has no list, reaches
// grep from its script, and the owner's own script reaches it too.
func TestToolSmithScriptOfAChildIsBoundByItsAgentsAllowListOnly(t *testing.T) {
	toolSmithInterpreter(t)
	scripter := agentWithTools("scripter", "tool_smith", "read", "subagent")
	free := agentNamed("free")
	var script, freeScript string
	f := newDenyFixture(t, nestingSettings(scripter, free), map[string]nestedBehaviour{
		"scripter": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			script = childCall(t, ctx, request, "tool_smith", `{"code":"const g = await tools.grep({}); const r = await tools.read({}); return g.status + ':' + g.summary + '|' + r.status;"}`)
			if out := childCall(t, ctx, request, "subagent", `{"task":"y","agent":"free"}`); !strings.Contains(out, "Agent free finished") {
				t.Errorf("scripter's child: %s", out)
			}
			return SubagentResult{Text: "done"}, nil
		},
		"free": func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
			freeScript = childCall(t, ctx, request, "tool_smith", grepScript)
			return SubagentResult{Text: "free done"}, nil
		},
	})
	if out := f.call(t, "subagent", `{"task":"x","agent":"scripter"}`); !strings.Contains(out, "Agent scripter finished") {
		t.Fatalf("subagent: %s", out)
	}
	if !strings.Contains(script, "invalid_input:tool grep not allowed; allowed: read|ok") {
		t.Fatalf("scripter's script reached grep, which its allow list leaves out, or lost read:\n%s", script)
	}
	if f.grepRan.Load() != 1 || !strings.Contains(freeScript, "ok:true") {
		t.Fatalf("the script of scripter's child, whose agent has no list, was narrowed by scripter's (grep ran %d):\n%s", f.grepRan.Load(), freeScript)
	}
	if out := f.call(t, "tool_smith", grepScript); f.grepRan.Load() != 2 || !strings.Contains(out, "ok:true") {
		t.Fatalf("the owner's own script lost grep (ran %d):\n%s", f.grepRan.Load(), out)
	}
}

// The bridge keeps what ExecutionContext.RunTools covers and nothing else, after the profile and the
// deny; nil narrows nothing.
func TestToolSmithBridgeKeepsOnlyTheRunsOwnTools(t *testing.T) {
	read := bridgeTestTool(t, "read", true, func(map[string]any) string { return "r" })
	grep := bridgeTestTool(t, "grep", true, func(map[string]any) string { return "g" })
	nested := bridgeNested(read, grep)
	settings := sessionfs.NativeToolSmithSettings{Tools: []string{"grep", "read"}}
	onlyRead := func(definition tools.Definition) bool { return definition.Name == "read" }
	for name, tc := range map[string]struct {
		deny     []string
		runTools func(tools.Definition) bool
		want     string
	}{
		"whole":           {nil, nil, "grep,read"},
		"run's own tools": {nil, onlyRead, "read"},
		"and a deny":      {[]string{"read"}, onlyRead, ""},
		"none":            {nil, func(tools.Definition) bool { return false }, ""},
	} {
		if got := strings.Join(poolNames(nested.allowed(settings, tc.deny, tc.runTools)), ","); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// No deny crosses a remote hop (U3 keeps the remote boundary): a remote agent and a remote profile
// get the very request a caller without a deny sends, and the remote server's own settings decide
// the tools there. Nothing in the request claims a narrowing.
func TestDenyCeilingIsNotSentToARemoteServer(t *testing.T) {
	denied := remoteCaller
	denied.Deny = []string{"exec", "mcp:docs:*"}

	sub := newRemoteSubagentFixture(t, peerGrant, nil)
	for index, execution := range []tools.ExecutionContext{remoteCaller, denied} {
		go func() { sub.peer.backend.run(t, index).finish(remoteapi.RunResult{State: "done", Text: "fine"}) }()
		if out := sub.start(t, remoteAgent, 5000, execution); !strings.Contains(out, "fine") {
			t.Fatalf("remote agent call %d:\n%s", index, out)
		}
	}
	subs := sub.peer.backend.subs
	if len(subs) == 2 {
		// Each start names a run id of its own.
		subs[0].RunID, subs[1].RunID = "", ""
	}
	if len(subs) != 2 || !reflect.DeepEqual(subs[0], subs[1]) {
		t.Fatalf("remote agent requests differ with a deny: %+v", subs)
	}

	message := newRemoteMessageFixture(t, peerGrant, nil)
	for index, execution := range []tools.ExecutionContext{remoteCaller, denied} {
		go func() { message.peer.backend.run(t, index).finish(remoteapi.RunResult{State: "done", Text: "fine"}) }()
		if out := message.send(t, remoteTarget, 5000, execution); !strings.Contains(out, "fine") {
			t.Fatalf("remote message %d:\n%s", index, out)
		}
	}
	if got := message.peer.backend.messages; len(got) != 2 || got[0] != got[1] {
		t.Fatalf("remote profile requests differ with a deny: %+v", got)
	}
}
