package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fakeRunMCP is a RunMCPLoader that records what it was asked and stops. Its tools answer with
// their number, so a test sees which start a call reached.
type fakeRunMCP struct {
	t        *testing.T
	mu       sync.Mutex
	requests []RunMCPRequest
	started  int
	closes   int
	err      error
	// servers are the stdio servers each start reports; "zvec" when empty.
	servers []string
	onClose func(RunMCPRequest)
}

func (f *fakeRunMCP) load(_ context.Context, request RunMCPRequest) (RunMCPServers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.err != nil {
		return RunMCPServers{}, f.err
	}
	f.started++
	number := f.started
	servers := f.servers
	if servers == nil {
		servers = []string{"zvec"}
	}
	tool := withExecutors(f.t, mcpStub(f.t, "zvec", "search"))[0]
	tool.Execute = nil
	tool.ExecuteOutput = func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
		return json.RawMessage(`"run-start-` + string(rune('0'+number)) + `"`), nil
	}
	return RunMCPServers{Tools: []tools.Tool{tool}, Servers: servers, Close: func() {
		f.mu.Lock()
		f.closes++
		f.mu.Unlock()
		if f.onClose != nil {
			f.onClose(request)
		}
	}}, nil
}

func (f *fakeRunMCP) counts() (loads, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests), f.closes
}

// buildIsolatedWithMCP is the subagent tool of the resolver's build: the agents' MCP tools are
// bound, and the loader (nil: none) is set.
func buildIsolatedWithMCP(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, load RunMCPLoader) *tools.Catalog {
	t.Helper()
	hook := &SubagentMCP{}
	parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(settings, parent, nil, launcher, hook, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	if load != nil {
		hook.SetRunMCP(load)
	}
	if err := hook.Bind([]tools.Tool{mcpStub(t, "zvec", "search"), mcpStub(t, "web", "fetch")}); err != nil {
		t.Fatal(err)
	}
	return catalogWithAsync(t, host, parent, tool)
}

func mcpAgent(agent sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentAgent {
	agent.Tools = append(agent.Tools, "mcp:zvec:*", "mcp:web:*")
	return agent
}

// asRun asks the route of a run's execution what it does with the tools of the two servers.
type routeAnswer struct {
	covered bool
	output  string
	err     error
}

func askRoute(t *testing.T, route tools.MCPRouter, server, name string) routeAnswer {
	t.Helper()
	return askRouteAs(t, route, tools.ExecutionContext{}, server, name)
}

// askRouteAs is askRoute for a caller whose execution is execution.
func askRouteAs(t *testing.T, route tools.MCPRouter, execution tools.ExecutionContext, server, name string) routeAnswer {
	t.Helper()
	if route == nil {
		return routeAnswer{}
	}
	tool, covered, err := route("", tools.Definition{ID: "mcp:" + server + ":" + name, Server: server}, execution)
	answer := routeAnswer{covered: covered, err: err}
	if covered && err == nil {
		output, callErr := tool.ExecuteOutput(context.Background(), nil, tools.ExecutionContext{})
		if callErr != nil {
			t.Fatal(callErr)
		}
		answer.output = string(output)
	}
	return answer
}

// An isolated run with MCP tools starts its stdio servers in its worktree, before the child
// runs; the child's calls to them go to those and a call to a server the run did not start (an
// HTTP one) does not; the servers stop when the child is over, before the worktree is
// collected and removed, and a call after that fails instead of reaching anything.
func TestIsolatedRunStartsItsMCPServersInItsWorktree(t *testing.T) {
	repo := gitRepository(t)
	loader := &fakeRunMCP{t: t}
	var events []string
	var workspace string
	var folderAtClose bool
	loader.onClose = func(request RunMCPRequest) {
		_, err := os.Stat(request.Workspace)
		folderAtClose = err == nil
		events = append(events, "servers stopped")
	}
	var inRun, outside routeAnswer
	var route tools.MCPRouter
	var childExecution tools.ExecutionContext
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		events = append(events, "child runs")
		workspace, route, childExecution = request.Execution.WorkspaceRoot, request.Execution.MCPRoute, request.Execution
		inRun, outside = askRouteAs(t, route, childExecution, "zvec", "search"), askRouteAs(t, route, childExecution, "web", "fetch")
		return SubagentResult{Text: "done"}, nil
	}}
	settings := writeSettings(mcpAgent(isolatedAgent("fixer")))
	settings.StorageDir = t.TempDir()
	catalog := buildIsolatedWithMCP(t, settings, launcher, loader.load)

	out := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"x","block_ms":3000}`)
	if len(loader.requests) != 1 {
		t.Fatalf("the loader was asked %d times, want once\n%s", len(loader.requests), out)
	}
	request := loader.requests[0]
	if request.Key != "" || request.Origin != repo || request.Workspace != workspace || !strings.Contains(workspace, filepath.Join(".git", "jad-worktrees")) {
		t.Fatalf("request %+v for a child working in %s of %s", request, workspace, repo)
	}
	if !inRun.covered || inRun.err != nil || inRun.output != `"run-start-1"` {
		t.Fatalf("a call to the run's stdio server: %+v", inRun)
	}
	if outside.covered {
		t.Fatalf("a call to a server the run did not start was covered: %+v", outside)
	}
	if strings.Join(events, ", ") != "child runs, servers stopped" || !folderAtClose {
		t.Fatalf("events %v, worktree there when the servers stopped: %v", events, folderAtClose)
	}
	if after := askRouteAs(t, route, childExecution, "zvec", "search"); !after.covered || !errors.Is(after.err, errRunMCPEnded) {
		t.Fatalf("a call after the run: %+v", after)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("worktree folder left behind: %v", err)
	}
}

// A server that does not start ends the run before its child runs: the run never falls back to
// the session's server, and nothing of it is left behind.
func TestIsolatedRunEndsWhenItsMCPServersDoNotStart(t *testing.T) {
	repo := gitRepository(t)
	loader := &fakeRunMCP{t: t, err: errors.New("MCP server zvec failed to start")}
	launcher := &fakeLauncher{}
	settings := writeSettings(mcpAgent(isolatedAgent("fixer")))
	settings.StorageDir = t.TempDir()
	catalog := buildIsolatedWithMCP(t, settings, launcher, loader.load)

	out := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"x","block_ms":3000}`)
	if !strings.Contains(out, "subagent_failed") || !strings.Contains(out, "the run's MCP servers could not be started: MCP server zvec failed to start") {
		t.Fatalf("result:\n%s", out)
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("the child ran %d times without its servers", len(launcher.requests))
	}
	if branches := gitCommand(t, repo, "branch", "--list", "jad/*", "--format=%(refname:short)"); branches != "" {
		t.Fatalf("a branch was left: %q", branches)
	}
	if entries, _ := os.ReadDir(filepath.Join(repo, ".git", "jad-worktrees")); len(entries) != 0 {
		t.Fatalf("worktree folders left: %v", entries)
	}
}

// A run that needs no servers of its own starts none: one in the session's folder, one without MCP
// tools, and a session without the loader.
func TestRunsThatNeedNoServersOfTheirOwnStartNone(t *testing.T) {
	repo := gitRepository(t)
	loader := &fakeRunMCP{t: t}
	var routes []tools.MCPRouter
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		routes = append(routes, request.Execution.MCPRoute)
		return SubagentResult{Text: "done"}, nil
	}}
	inFolder := mcpAgent(writerAgent("inplace", "read", "exec"))
	settings := writeSettings(inFolder, isolatedAgent("noservers"), mcpAgent(isolatedAgent("fixer")))
	settings.StorageDir = t.TempDir()
	catalog := buildIsolatedWithMCP(t, settings, launcher, loader.load)
	for _, agent := range []string{"inplace", "noservers"} {
		worktreeCall(t, catalog, repo, `{"agent":"`+agent+`","task":"x","block_ms":3000}`)
	}
	if loads, _ := loader.counts(); loads != 0 {
		t.Fatalf("%d starts for runs that need none", loads)
	}
	if routes[0] != nil {
		t.Fatal("a run in the session's folder got a route")
	}
	if got := askRoute(t, routes[1], "zvec", "search"); got.covered {
		t.Fatalf("a run without MCP tools has servers: %+v", got)
	}
	// Without the loader the isolated agent runs on the session's servers, as before P6.
	routes = nil
	catalog = buildIsolatedWithMCP(t, settings, launcher, nil)
	worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"x","block_ms":3000}`)
	if got := askRoute(t, routes[0], "zvec", "search"); got.covered {
		t.Fatalf("without a loader the run's calls were covered: %+v", got)
	}
}

// A resumed run is a new run: it starts servers of its own and does not reuse the first run's.
func TestResumedIsolatedRunStartsFreshMCPServers(t *testing.T) {
	repo := gitRepository(t)
	loader := &fakeRunMCP{t: t}
	var workspaces []string
	launcher := &fakeLauncher{run: func(_ context.Context, request SubagentRequest) (SubagentResult, error) {
		workspaces = append(workspaces, request.Execution.WorkspaceRoot)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		request.OnTranscript(dir)
		// A change, so the result names the run's branch.
		if err := os.WriteFile(filepath.Join(request.Execution.WorkspaceRoot, "n"+filepath.Base(request.Execution.WorkspaceRoot)+".txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return SubagentResult{Text: "done"}, nil
	}}
	settings := writeSettings(mcpAgent(isolatedAgent("fixer")))
	settings.StorageDir = t.TempDir()
	catalog := buildIsolatedWithMCP(t, settings, launcher, loader.load)
	first := worktreeCall(t, catalog, repo, `{"agent":"fixer","task":"one","block_ms":3000}`)
	run := runIDOf(t, first)
	second := worktreeCall(t, catalog, repo, `{"resume":"`+run+`","task":"two","block_ms":3000}`)
	if !strings.Contains(second, "resumed_from: "+run) {
		t.Fatalf("second result:\n%s", second)
	}
	loads, closes := loader.counts()
	if loads != 2 || closes != 2 || len(workspaces) != 2 || loader.requests[0].Workspace == loader.requests[1].Workspace {
		t.Fatalf("starts %d, stops %d, workspaces %v, requests %+v", loads, closes, workspaces, loader.requests)
	}
}

// A child that works inside an isolated run's worktree (one nested in the run, not isolated
// itself) uses the run's servers: the set of one key starts once, another key starts its own, and a
// run in any other folder starts nothing. They all stop with the run.
func TestChildInAnIsolatedRunSharesTheRunsServers(t *testing.T) {
	root, tree := isolatedTree(t, "sub_nested")
	_, mcp := openRunMCP(tree, root)
	loader := &fakeRunMCP{t: t}
	inRun := tools.ExecutionContext{WorkspaceRoot: tree.Workspace}
	for _, key := range []string{"", "", "set-a", "set-a"} {
		if err := startRunMCP(context.Background(), inRun, key, loader.load); err != nil {
			t.Fatal(err)
		}
	}
	if err := startRunMCP(context.Background(), tools.ExecutionContext{WorkspaceRoot: root}, "", loader.load); err != nil {
		t.Fatal(err)
	}
	if loads, closes := loader.counts(); loads != 2 || closes != 0 {
		t.Fatalf("starts %d, stops %d, want 2 and 0", loads, closes)
	}
	if loader.requests[0].Workspace != tree.Workspace || loader.requests[1].Key != "set-a" {
		t.Fatalf("requests %+v", loader.requests)
	}
	forgetIsolatedRun(tree)
	if loads, closes := loader.counts(); loads != 2 || closes != 2 {
		t.Fatalf("after the run: starts %d, stops %d", loads, closes)
	}
	if err := mcp.ensure(context.Background(), "late", inRun, loader.load); !errors.Is(err, errRunMCPEnded) {
		t.Fatalf("a start after the run ended = %v", err)
	}
}

// A start that ends after the run did stops its servers at once, and a run's end stops every
// set once however many callers end it.
func TestRunMCPStopsEveryStartedSetOnce(t *testing.T) {
	loader := &fakeRunMCP{t: t}
	mcp := newRunMCP("/tree", "/repo")
	late, entered := make(chan struct{}), make(chan struct{})
	slow := func(ctx context.Context, request RunMCPRequest) (RunMCPServers, error) {
		close(entered)
		<-late
		return loader.load(ctx, request)
	}
	if err := mcp.ensure(context.Background(), "a", tools.ExecutionContext{}, loader.load); err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- mcp.ensure(context.Background(), "b", tools.ExecutionContext{}, slow) }()
	<-entered
	var ending sync.WaitGroup
	for range 4 {
		ending.Add(1)
		go func() { defer ending.Done(); mcp.end() }()
	}
	ending.Wait()
	close(late)
	if err := <-failed; !errors.Is(err, errRunMCPEnded) {
		t.Fatalf("the start that outlived the run = %v", err)
	}
	if loads, closes := loader.counts(); loads != 2 || closes != 2 {
		t.Fatalf("starts %d, stops %d, want each started set stopped once", loads, closes)
	}
	var nilRun *runMCP
	nilRun.end()
}

// A route covers the stdio servers of a started set only, and answers for a tool the server no
// longer lists with an error that names it, never with the session's tool.
func TestRunMCPRouteCoversOnlyWhatItStarted(t *testing.T) {
	loader := &fakeRunMCP{t: t}
	mcp := newRunMCP("/tree", "/repo")
	if answer := askRoute(t, mcp.route, "zvec", "search"); answer.covered {
		t.Fatal("a set that was not started is covered")
	}
	if err := mcp.ensure(context.Background(), "", tools.ExecutionContext{}, loader.load); err != nil {
		t.Fatal(err)
	}
	if answer := askRoute(t, mcp.route, "zvec", "search"); !answer.covered || answer.err != nil {
		t.Fatalf("a started server: %+v", answer)
	}
	if answer := askRoute(t, mcp.route, "web", "fetch"); answer.covered {
		t.Fatalf("a server the set does not hold: %+v", answer)
	}
	gone := askRoute(t, mcp.route, "zvec", "reindex")
	if !gone.covered || gone.err == nil || !strings.Contains(gone.err.Error(), "mcp:zvec:reindex") {
		t.Fatalf("a tool the run's server does not offer: %+v", gone)
	}
	// Another key's tools are not this key's.
	if _, covered, _ := mcp.route("set-b", tools.Definition{ID: "mcp:zvec:search", Server: "zvec"}, tools.ExecutionContext{}); covered {
		t.Fatal("a set of another key is covered")
	}
}

// A delegated profile run gets servers of its own in its worktree, stopped before the worktree is
// collected; its route is on the execution its tools run with.
func TestDelegatedRunStartsAndStopsItsMCPServers(t *testing.T) {
	repo := gitRepository(t)
	execution := tools.ExecutionContext{WorkspaceRoot: repo}
	opened, err := OpenDelegatedWorktree(context.Background(), nil, sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}, &execution, t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	loader := &fakeRunMCP{t: t}
	var stoppedWith bool
	loader.onClose = func(request RunMCPRequest) {
		_, err := os.Stat(request.Workspace)
		stoppedWith = err == nil
	}
	if err := opened.StartMCP(context.Background(), execution, loader.load); err != nil {
		t.Fatal(err)
	}
	if len(loader.requests) != 1 || loader.requests[0].Workspace != execution.WorkspaceRoot || loader.requests[0].Origin != repo {
		t.Fatalf("requests %+v for a run in %s", loader.requests, execution.WorkspaceRoot)
	}
	if answer := askRouteAs(t, execution.MCPRoute, execution, "zvec", "search"); !answer.covered || answer.output != `"run-start-1"` {
		t.Fatalf("a call of the delegated run: %+v", answer)
	}
	opened.Close(context.Background(), execution, "m")
	if _, closes := loader.counts(); closes != 1 || !stoppedWith {
		t.Fatalf("stops %d, worktree there when they stopped: %v", closes, stoppedWith)
	}
	if answer := askRouteAs(t, execution.MCPRoute, execution, "zvec", "search"); !answer.covered || !errors.Is(answer.err, errRunMCPEnded) {
		t.Fatalf("a call after the run: %+v", answer)
	}
	if err := (*DelegatedWorktree)(nil).StartMCP(context.Background(), execution, loader.load); err != nil {
		t.Fatalf("a nil worktree: %v", err)
	}
}

// A run's servers are asked for by the key of the set its agent's MCP tools were listed from: the
// session's (empty), the agent's toolset, or the toolset in the agent's own sandbox.
func TestRunMCPPlanNamesTheServerSetOfTheAgent(t *testing.T) {
	boxed := toolsetAgent("boxed", "mcp:zvec:search")
	boxed.Sandbox = &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", Tools: []string{"mcp"}}
	hook := &SubagentMCP{}
	parent := withExecutors(t, readOnlyTool("read"))
	if _, err := newSubagentTool(subagentSettings(toolsetAgent("finder", "mcp:zvec:search"), boxed, agentWithTools("plain", "read", "mcp:browser:click"), agentWithTools("native-only", "read")), parent,
		map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, &fakeLauncher{}, hook, nil); err != nil {
		t.Fatal(err)
	}
	loader := &fakeRunMCP{t: t}
	hook.SetRunMCP(loader.load)
	toolsetTool := mcpStub(t, "zvec", "search")
	if err := hook.BindToolsets([]tools.Tool{mcpStub(t, "browser", "click")}, map[string]ToolsetMCP{
		searchToolset:        {Tools: []tools.Tool{toolsetTool}},
		ToolsetMCPKey(boxed): {Tools: []tools.Tool{toolsetTool}},
	}); err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]*string{"finder": ptr(searchToolset), "boxed": ptr(searchToolset + "\x00agent\x00boxed"), "plain": ptr(""), "native-only": nil} {
		selected, problem := hook.tool.selectAgent(map[string]any{"agent": agent})
		if problem != "" {
			t.Fatal(problem)
		}
		plan := hook.tool.runMCPFor(selected)
		switch {
		case want == nil && plan != nil:
			t.Errorf("%s: a plan for an agent without MCP tools: %+v", agent, plan)
		case want != nil && (plan == nil || plan.key != *want):
			t.Errorf("%s: plan %+v, want key %q", agent, plan, *want)
		}
	}
}

func ptr(text string) *string { return &text }

// A run's servers belong to the sandbox they start in: a child with a narrower sandbox than the
// run's first agent gets servers of its own, started under its policy, and neither calls the
// other's. An agent whose workspace and sandbox are the same (the same content, not the same value)
// shares the set, and a caller no set was started for fails, never reaching another agent's
// servers or the session's.
func TestNarrowerChildOfAnIsolatedRunGetsServersOfItsOwnSandbox(t *testing.T) {
	root, tree := isolatedTree(t, "sub_narrow")
	route, _ := openRunMCP(tree, root)
	loader := &fakeRunMCP{t: t}
	broad := &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", Tools: []string{"exec", "mcp"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}"}}
	narrow, err := sessionfs.NarrowSandbox(broad, &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}/src"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tools.ExecutionContext{WorkspaceRoot: tree.Workspace, Environment: &tools.Environment{Sandbox: broad}}
	child := tools.ExecutionContext{WorkspaceRoot: tree.Workspace, Environment: &tools.Environment{Sandbox: narrow}}
	unboxed := tools.ExecutionContext{WorkspaceRoot: tree.Workspace}

	if err := startRunMCP(context.Background(), parent, "", loader.load); err != nil {
		t.Fatal(err)
	}
	// The child does not use the parent's set: until its own starts, its call fails.
	if answer := askRouteAs(t, route, child, "zvec", "search"); !answer.covered || answer.err == nil || !strings.Contains(answer.err.Error(), "was not started for this agent") {
		t.Fatalf("a narrower child before its servers started: %+v", answer)
	}
	if err := startRunMCP(context.Background(), child, "", loader.load); err != nil {
		t.Fatal(err)
	}
	if loads, _ := loader.counts(); loads != 2 {
		t.Fatalf("a narrower child shared the parent's servers: %d starts, want 2", loads)
	}
	if got := loader.requests[0].Environment.Sandbox; got != broad {
		t.Fatalf("the parent's servers were asked for in %+v", got)
	}
	if got := loader.requests[1].Environment.Sandbox; got != narrow || len(got.WritablePaths) != 1 || got.WritablePaths[0] != "{workspace}/src" {
		t.Fatalf("the child's servers were asked for in %+v, want the child's narrowed sandbox", got)
	}
	if answer := askRouteAs(t, route, parent, "zvec", "search"); answer.output != `"run-start-1"` {
		t.Fatalf("the parent reached %+v", answer)
	}
	if answer := askRouteAs(t, route, child, "zvec", "search"); answer.output != `"run-start-2"` {
		t.Fatalf("the child reached %+v", answer)
	}
	// The same sandbox by content shares; so do siblings that ask again, and a resume's child.
	twin := tools.ExecutionContext{WorkspaceRoot: tree.Workspace, Environment: &tools.Environment{Sandbox: func() *sessionfs.SandboxSettings { c := *narrow; return &c }()}}
	for _, execution := range []tools.ExecutionContext{child, twin, child} {
		if err := startRunMCP(context.Background(), execution, "", loader.load); err != nil {
			t.Fatal(err)
		}
	}
	if loads, _ := loader.counts(); loads != 2 {
		t.Fatalf("agents with the same sandbox started %d sets, want them to share 2", loads)
	}
	if answer := askRouteAs(t, route, twin, "zvec", "search"); answer.output != `"run-start-2"` {
		t.Fatalf("a twin of the child reached %+v", answer)
	}
	// An agent without a sandbox is not an agent with one, in either order.
	if answer := askRouteAs(t, route, unboxed, "zvec", "search"); !answer.covered || answer.err == nil {
		t.Fatalf("an agent without a sandbox reached a sandboxed set: %+v", answer)
	}
	if err := startRunMCP(context.Background(), unboxed, "", loader.load); err != nil {
		t.Fatal(err)
	}
	if loads, _ := loader.counts(); loads != 3 || loader.requests[2].Environment != nil {
		t.Fatalf("an agent without a sandbox: %d starts, request %+v", loads, loader.requests[2])
	}
	// Another workspace is another set (a nested isolated run has its own run and servers).
	elsewhere := tools.ExecutionContext{WorkspaceRoot: filepath.Join(tree.Workspace, "sub"), Environment: parent.Environment}
	if answer := askRouteAs(t, route, elsewhere, "zvec", "search"); !answer.covered || answer.err == nil {
		t.Fatalf("a caller in another workspace reached %+v", answer)
	}
	// Every set stops with the run, and a call after it fails for each of them.
	forgetIsolatedRun(tree)
	if loads, closes := loader.counts(); loads != 3 || closes != 3 {
		t.Fatalf("after the run: starts %d, stops %d", loads, closes)
	}
	for _, execution := range []tools.ExecutionContext{parent, child, unboxed} {
		if answer := askRouteAs(t, route, execution, "zvec", "search"); !errors.Is(answer.err, errRunMCPEnded) {
			t.Fatalf("a call after the run: %+v", answer)
		}
	}
}

// The server set key is part of what a set is for: one workspace and sandbox with several keys is
// several sets.
func TestRunMCPSetsOfOneSandboxAreSeparateByKey(t *testing.T) {
	mcp := newRunMCP("/tree", "/repo")
	loader := &fakeRunMCP{t: t}
	execution := tools.ExecutionContext{WorkspaceRoot: "/tree", Environment: &tools.Environment{Sandbox: &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"mcp"}}}}
	for _, key := range []string{"", "a", "b", "a"} {
		if err := mcp.ensure(context.Background(), key, execution, loader.load); err != nil {
			t.Fatal(err)
		}
	}
	if loads, _ := loader.counts(); loads != 3 {
		t.Fatalf("%d starts for three keys, want 3", loads)
	}
}

// Plan mode refuses a sub-agent call whose agent changesWorkspace. An isolated agent with MCP
// tools stays one of those whenever it can change files: its servers start in its worktree, but a
// streamable HTTP server is remote and a stdio server of a sandbox without mcp is not confined, so
// plan mode is not given a writer to run on the strength of a worktree. Without MCP tools, or with a
// tools list that has no tool that changes files, the worktree makes it a reader as before.
func TestIsolatedAgentWithMCPToolsStaysAWriterForPlanMode(t *testing.T) {
	mcpTool := []tools.Tool{{Definition: tools.Definition{ID: "mcp:s:t", Source: "mcp"}}}
	isolated := sessionfs.NativeSubagentAgent{Name: "a", Isolation: sessionfs.SubagentIsolationWorktree}
	listed := isolated
	listed.HasTools, listed.Tools = true, []string{"mcp:s:t"}
	for _, test := range []struct {
		name  string
		agent subagentAgent
		want  bool
	}{
		{"isolated, no MCP tools", subagentAgent{config: isolated}, false},
		{"isolated, MCP tools, can change files", subagentAgent{config: isolated, mcpTools: mcpTool}, true},
		{"isolated, MCP tools, list without a change tool", subagentAgent{config: listed, mcpTools: mcpTool}, false},
	} {
		if got := test.agent.changesWorkspace(); got != test.want {
			t.Errorf("%s: changesWorkspace = %v, want %v", test.name, got, test.want)
		}
	}
}
