package native

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// planMCPFixture is a subagent tool with MCP tools bound, as the resolver builds it, and a launcher
// that records each child it starts and lets a test run code as that child (the child's own calls).
type planMCPFixture struct {
	catalog  *tools.Catalog
	hook     *SubagentMCP
	launcher *fakeLauncher
	mu       sync.Mutex
	started  []string
	behave   map[string]func(context.Context, SubagentRequest) string
	outputs  map[string]string
}

// planMCPAgents: mcponly lists one MCP tool that may write, mcpread only a ReadOnly one, plain has no
// list (so every tool, and the subagent tool while max_depth allows), nester may nest and lists no
// MCP tool, fixer lists exec.
func newPlanMCPFixture(t *testing.T) *planMCPFixture {
	t.Helper()
	f := &planMCPFixture{behave: map[string]func(context.Context, SubagentRequest) string{}, outputs: map[string]string{}}
	f.launcher = &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		f.mu.Lock()
		f.started = append(f.started, request.Agent.Name)
		behave := f.behave[request.Agent.Name]
		f.mu.Unlock()
		if behave != nil {
			if out := behave(ctx, request); out != "" {
				f.mu.Lock()
				f.outputs[request.Agent.Name] += out
				f.mu.Unlock()
			}
		}
		return SubagentResult{Text: "done"}, nil
	}}
	settings := subagentSettings(
		agentWithTools("mcponly", "mcp:zvec:reindex"),
		agentWithTools("mcpread", "mcp:zvec:resources"),
		agentNamed("plain"),
		agentWithTools("nester", "read", "subagent"),
		agentWithTools("fixer", "read", "exec"),
	)
	settings.MaxDepth, settings.MaxConcurrent, settings.MaxRunsPerRoot, settings.MaxRunsPerSession = 3, 8, 16, 32
	// Built as buildWithMCP does, with the hook kept to reach the tool's manager.
	f.hook = &SubagentMCP{}
	parent := withExecutors(t, readOnlyTool("read"), writeTool("exec"))
	tool, err := NewSubagentToolWithMCP(settings, parent, f.launcher, f.hook)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hook.Bind([]tools.Tool{mcpStub(t, "zvec", "reindex"), withReadOnly(mcpStub(t, "zvec", "resources"))}); err != nil {
		t.Fatal(err)
	}
	if f.catalog, err = tools.NewCatalog(append(parent, tool)); err != nil {
		t.Fatal(err)
	}
	return f
}

func withReadOnly(tool tools.Tool) tools.Tool {
	tool.ReadOnly = true
	return tool
}

// lastRun is the id of the newest run the session started.
func (f *planMCPFixture) lastRun(t *testing.T) string {
	t.Helper()
	views := f.hook.tool.manager.list(testSessionKey)
	if len(views) == 0 {
		t.Fatal("the call made no run")
	}
	newest := views[0]
	for _, view := range views {
		if view.startedAt.After(newest.startedAt) {
			newest = view
		}
	}
	return newest.id
}

func (f *planMCPFixture) startedAgents() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.started, ",")
}

func subagentFunctionCall(arguments string) json.RawMessage {
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	return call
}

func callOutput(t *testing.T, catalog *tools.Catalog, call json.RawMessage, execution tools.ExecutionContext) string {
	t.Helper()
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

func isPlanRefusal(output string) bool {
	return strings.Contains(output, tools.PlanModeErrorCode) && strings.Contains(output, "forbidden")
}

var planExecution = tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey}

// An agent with an MCP tool that is not marked ReadOnly is a writer for scheduling and the
// checkpoint, whatever its native tools: its list may name only MCP tools, or no list at all,
// in a session with no exec. Only ReadOnly MCP tools leave it a reader.
func TestSubagentWithAWriteCapableMCPToolIsAWriterForSchedulingAndCheckpoint(t *testing.T) {
	f := newPlanMCPFixture(t)
	for agent, want := range map[string]bool{
		"mcponly": true,  // an explicit list of one MCP tool
		"plain":   true,  // no list: every tool of the pool
		"mcpread": false, // only a ReadOnly MCP tool
		"nester":  true,  // it may nest and a writer is reachable (subagent_nested_writer_test.go)
		"fixer":   true,
	} {
		call := subagentFunctionCall(`{"task":"x","agent":"` + agent + `"}`)
		if got := f.catalog.CallMayChangeFiles(call); got != want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v (this is what the checkpoint reads)", agent, got, want)
		}
		if got := f.catalog.ChangesState(call); got != want {
			t.Errorf("%s: ChangesState = %v, want %v (this is what ordered scheduling reads)", agent, got, want)
		}
	}
}

// A MCP-only agent is a writer in a worktree of its own and in a sandbox that writes only scratch
// too (D10), and a writer is handed off as a writer: not as a reader handle.
func TestMCPOnlySubagentIsAWriterHandledLikeOne(t *testing.T) {
	mcponly := agentWithTools("mcponly", "mcp:zvec:reindex")
	mcponly.NoChangeTools = true
	isolated := mcponly
	isolated.Isolation = sessionfs.SubagentIsolationWorktree
	for name, agent := range map[string]subagentAgent{
		"explicit list":    {config: mcponly, mcpTools: []tools.Tool{mcpStub(t, "zvec", "reindex")}},
		"isolated":         {config: isolated, mcpTools: []tools.Tool{mcpStub(t, "zvec", "reindex")}},
		"unlisted, no MCP": {config: agentNamed("plain"), mcpTools: []tools.Tool{mcpStub(t, "zvec", "reindex")}},
	} {
		agent.config.NoChangeTools = true
		if !agent.changesWorkspace() || !agent.changesFiles() {
			t.Errorf("%s: changesWorkspace %v changesFiles %v, want both", name, agent.changesWorkspace(), agent.changesFiles())
		}
		if !backgroundSafe(sessionfs.NativeSubagentSettings{BackgroundWriters: true}, agent, &AsyncHost{}) {
			t.Errorf("%s: a writer must take the background-writer path, not the reader handle", name)
		}
	}
	reader := subagentAgent{config: agentNamed("r"), mcpTools: []tools.Tool{mcpReadOnlyTool("lookup")}}
	reader.config.NoChangeTools = true
	if reader.changesWorkspace() || reader.changesFiles() {
		t.Error("an agent with only ReadOnly MCP tools must stay a reader")
	}
}

// Plan mode refuses a call to an agent with a write-capable MCP tool and runs the legitimate
// read-only ones; an explicit plan_mode.allow_tools entry keeps the tool whole.
func TestPlanModeRefusesMCPCapableSubagentsAndKeepsReaders(t *testing.T) {
	f := newPlanMCPFixture(t)
	plan, err := f.catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"mcponly", "plain", "fixer"} {
		if out := callOutput(t, plan, subagentFunctionCall(`{"task":"x","agent":"`+agent+`"}`), planExecution); !isPlanRefusal(out) {
			t.Errorf("plan mode ran %s: %s", agent, out)
		}
	}
	if started := f.startedAgents(); started != "" {
		t.Fatalf("plan mode started %q", started)
	}
	for _, agent := range []string{"mcpread", "nester"} {
		if out := callOutput(t, plan, subagentFunctionCall(`{"task":"x","agent":"`+agent+`"}`), planExecution); isPlanRefusal(out) {
			t.Errorf("plan mode refused the reader %s: %s", agent, out)
		}
	}
	if started := f.startedAgents(); started != "mcpread,nester" {
		t.Fatalf("started %q, want the two readers", started)
	}
	if !plan.IsPlanGuardedCall(subagentFunctionCall(`{"task":"x","agent":"mcponly"}`)) {
		t.Fatal("a refused plan-mode call must be guarded, so it takes no checkpoint")
	}

	allowed, err := f.catalog.PlanMode([]string{"subagent"})
	if err != nil {
		t.Fatal(err)
	}
	if out := callOutput(t, allowed, subagentFunctionCall(`{"task":"x","agent":"mcponly"}`), planExecution); isPlanRefusal(out) {
		t.Errorf("an allow entry for subagent must keep it whole: %s", out)
	}
	if allowed.IsPlanGuardedCall(subagentFunctionCall(`{"task":"x","agent":"mcponly"}`)) {
		t.Error("an allowed tool is not guarded, so its calls still take the checkpoint")
	}
}

// A resume that names no agent is judged by the agent of the run it continues: a writer run cannot
// be resumed in plan mode (or scheduled as a reader) by leaving agent out.
func TestSubagentResumeWithoutAgentIsJudgedByTheSourceRunsAgent(t *testing.T) {
	f := newPlanMCPFixture(t)
	callOutput(t, f.catalog, subagentFunctionCall(`{"task":"first","agent":"mcponly"}`), planExecution)
	id := f.lastRun(t)
	resume := subagentFunctionCall(`{"task":"again","resume":"` + id + `"}`)
	if !f.catalog.CallMayChangeFiles(resume) || !f.catalog.ChangesState(resume) {
		t.Fatal("a resume of a writer run, naming no agent, is a reader call")
	}
	plan, err := f.catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := f.startedAgents()
	if out := callOutput(t, plan, resume, planExecution); !isPlanRefusal(out) {
		t.Fatalf("plan mode resumed a writer run: %s", out)
	}
	if f.startedAgents() != before {
		t.Fatal("the refused resume started a run")
	}
	// A reader run resumes as a reader.
	callOutput(t, f.catalog, subagentFunctionCall(`{"task":"first","agent":"mcpread"}`), planExecution)
	readerResume := subagentFunctionCall(`{"task":"again","resume":"` + f.lastRun(t) + `"}`)
	if f.catalog.CallMayChangeFiles(readerResume) {
		t.Fatal("a resume of a reader run is a writer call")
	}
}

// Plan mode is enforced below the top call too: a reader child that may nest cannot start a writer
// child, one with an MCP tool included, at any depth, and what it may start still runs.
func TestPlanModeAppliesToNestedChildren(t *testing.T) {
	f := newPlanMCPFixture(t)
	nestedCalls := func(request SubagentRequest) string {
		var out strings.Builder
		for _, agent := range []string{"mcponly", "fixer", "mcpread", "plain"} {
			text := childCall(t, context.Background(), request, "subagent", `{"task":"deeper","agent":"`+agent+`"}`)
			out.WriteString(agent + "=" + map[bool]string{true: "refused", false: "ran"}[isPlanRefusal(text)] + ";")
		}
		return out.String()
	}
	f.behave["nester"] = func(_ context.Context, request SubagentRequest) string { return nestedCalls(request) }
	plan, err := f.catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	callOutput(t, plan, subagentFunctionCall(`{"task":"x","agent":"nester"}`), planExecution)
	want := "mcponly=refused;fixer=refused;mcpread=ran;plain=refused;"
	if got := f.outputs["nester"]; got != want {
		t.Fatalf("nested calls in plan mode: %q, want %q", got, want)
	}
	if started := f.startedAgents(); started != "nester,mcpread" {
		t.Fatalf("started %q: a refused nested call started a run", started)
	}

	// The grandchild: the allowed reader grandchild's own nested calls are refused the same way.
	f2 := newPlanMCPFixture(t)
	f2.behave["nester"] = func(_ context.Context, request SubagentRequest) string {
		childCall(t, context.Background(), request, "subagent", `{"task":"deeper","agent":"plain"}`)
		return ""
	}
	f2.behave["plain"] = func(_ context.Context, request SubagentRequest) string {
		return map[bool]string{true: "refused", false: "ran"}[isPlanRefusal(childCall(t, context.Background(), request, "subagent", `{"task":"deepest","agent":"mcponly"}`))]
	}
	// plain is a writer by its MCP tools, so plan mode refuses it before it can start a grandchild.
	plan2, _ := f2.catalog.PlanMode(nil)
	callOutput(t, plan2, subagentFunctionCall(`{"task":"x","agent":"nester"}`), planExecution)
	if strings.Contains(f2.startedAgents(), "plain") || strings.Contains(f2.startedAgents(), "mcponly") {
		t.Fatalf("a writer started below a plan-mode run: %s", f2.startedAgents())
	}

	// Outside plan mode nothing is refused at any depth.
	f3 := newPlanMCPFixture(t)
	f3.behave["nester"] = func(_ context.Context, request SubagentRequest) string { return nestedCalls(request) }
	callOutput(t, f3.catalog, subagentFunctionCall(`{"task":"x","agent":"nester"}`), planExecution)
	if got := f3.outputs["nester"]; got != "mcponly=ran;fixer=ran;mcpread=ran;plain=ran;" {
		t.Fatalf("nested calls outside plan mode: %q", got)
	}
}

// A grandchild below an allowed (whole) tool is not guarded: the allow entry's meaning does not
// change with depth.
func TestPlanModeAllowEntryKeepsNestedCallsWhole(t *testing.T) {
	f := newPlanMCPFixture(t)
	f.behave["nester"] = func(_ context.Context, request SubagentRequest) string {
		return childCall(t, context.Background(), request, "subagent", `{"task":"deeper","agent":"mcponly"}`)
	}
	allowed, err := f.catalog.PlanMode([]string{"subagent"})
	if err != nil {
		t.Fatal(err)
	}
	callOutput(t, allowed, subagentFunctionCall(`{"task":"x","agent":"nester"}`), planExecution)
	if isPlanRefusal(f.outputs["nester"]) || !strings.Contains(f.startedAgents(), "mcponly") {
		t.Fatalf("an allowed subagent tool was refused below its first call: %q started %q", f.outputs["nester"], f.startedAgents())
	}
}

// A remote agent is not told the caller is in plan mode, so plan mode does not start one, at any
// depth. Outside plan mode a remote call is still no local checkpoint or limit change.
func TestPlanModeDoesNotStartARemoteAgent(t *testing.T) {
	f := newRemoteSubagentFixture(t, peerGrant, nil)
	arguments := map[string]any{"task": "x", "agent": remoteAgent}
	if reason := f.tool.PlanModeRefusal(arguments, nil); reason == "" {
		t.Fatal("plan mode must refuse a remote agent")
	}
	if f.tool.PlanModeRefusal(map[string]any{"task": " ", "agent": remoteAgent}, nil) != "" {
		t.Error("listing the agents is not a start")
	}
	if f.tool.PlanModeRefusal(map[string]any{"task": "x", "agent": "explore"}, nil) != "" {
		t.Error("a local reader agent must still run in plan mode")
	}
	catalog, err := tools.NewCatalog([]tools.Tool{f.tool})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	call := subagentFunctionCall(`{"task":"x","agent":"` + remoteAgent + `"}`)
	if out := callOutput(t, plan, call, remoteCaller); !isPlanRefusal(out) {
		t.Fatalf("plan mode started a remote agent: %s", out)
	}
	// Below a guarded call (a nested child): the tool's own check refuses it.
	nested := remoteCaller
	nested.AgentDepth, nested.PlanMode, nested.PlanGuarded = 1, true, true
	if out := f.start(t, remoteAgent, 5000, nested); !strings.Contains(out, tools.PlanModeErrorCode) {
		t.Fatalf("a nested remote start in plan mode ran: %s", out)
	}
	if len(f.peer.backend.subs) != 0 {
		t.Fatal("a refused call reached the peer")
	}
	// An allow entry keeps it whole, as it does for any tool; and outside plan mode it runs.
	allowed, _ := catalog.PlanMode([]string{"subagent"})
	if out := callOutput(t, allowed, call, remoteCaller); isPlanRefusal(out) {
		t.Fatalf("an allow entry must keep the tool whole: %s", out)
	}
}

// The unrestricted agent of a session with no change tool (no exec) is judged by its MCP tools
// alone: a reader without them, a writer with one that may write, a reader with ReadOnly ones.
func TestUnrestrictedSubagentOfASessionWithoutExecIsJudgedByItsMCPTools(t *testing.T) {
	for name, tc := range map[string]struct {
		mcp  []tools.Tool
		want bool
	}{
		"no MCP tool":        {nil, false},
		"write-capable tool": {[]tools.Tool{mcpStub(t, "zvec", "reindex")}, true},
		"ReadOnly tool only": {[]tools.Tool{withReadOnly(mcpStub(t, "zvec", "resources"))}, false},
		"both":               {[]tools.Tool{withReadOnly(mcpStub(t, "zvec", "resources")), mcpStub(t, "zvec", "reindex")}, true},
	} {
		catalog, err := buildWithMCP(t, subagentSettings(agentNamed("plain")), &fakeLauncher{}, tc.mcp, readOnlyTool("read"))
		if err != nil {
			t.Fatal(err)
		}
		call := subagentFunctionCall(`{"task":"x","agent":"plain"}`)
		if got := catalog.CallMayChangeFiles(call); got != tc.want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", name, got, tc.want)
		}
		plan, err := catalog.PlanMode(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := isPlanRefusal(callOutput(t, plan, call, planExecution)); got != tc.want {
			t.Errorf("%s: plan mode refused = %v, want %v", name, got, tc.want)
		}
	}
}
