package native

import (
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// closureOf is the identity of a tool's executor: the address of the function value, which differs
// for every wrapper made (a gate or a hook is a new closure) and is the same for copies of one Tool.
func closureOf(tool tools.Tool) uintptr {
	if tool.ExecuteOutput != nil {
		execute := tool.ExecuteOutput
		return *(*uintptr)(unsafe.Pointer(&execute))
	}
	execute := tool.Execute
	return *(*uintptr)(unsafe.Pointer(&execute))
}

// copiesFixture is a subagent tool bound the way catalog assembly binds it: the owner's lazy set
// binds the session's MCP tools, NewMCPCopies gates and hooks them once (the owner's catalog holds
// copies.Session()), and BindCopies gives the agents the same copies. The owner's mcp:docs:search is
// lazy, and the toolset has its own tool with that ID. s1 and s2 use the session's servers, t1 and t2
// one toolset; every hook matches "*", so every copy is a wrapper of its own.
type copiesFixture struct {
	mcp                        *SubagentMCP
	copies                     MCPCopies
	ownerCalls, toolsetCalls   *int
	ownerMarker, toolsetMarker string
}

func newCopiesFixture(t *testing.T, bindFirst bool) copiesFixture {
	t.Helper()
	var ownerCalls, toolsetCalls int
	search := sameIDStub(t, "owner description", "ownerarg", &ownerCalls)
	look := mcpStub(t, "docs", "look")
	toolsetSearch := sameIDStub(t, "toolset description", "toolsetarg", &toolsetCalls)
	dir := t.TempDir()
	ownerMarker, toolsetMarker := filepath.Join(dir, "owner"), filepath.Join(dir, "toolset")
	ownerHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("owner", "*", "echo fired >> "+ownerMarker)}, nil)
	toolsetHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("toolset", "*", "echo fired >> "+toolsetMarker)}, nil)

	set := tools.NewLazySet([]string{"mcp:docs:search"}, 256, nil)
	session := []tools.Tool{search, look}
	set.Bind(session)
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	settings := choiceSettings(nestingAgent("s1", "read", "subagent", "load_tool", "mcp:docs:search", "mcp:docs:look"), nestingAgent("s2", "read", "load_tool", "mcp:docs:search", "mcp:docs:look"),
		toolsetAgent("t1", "grep", "load_tool", "mcp:docs:search"), toolsetAgent("t2", "grep", "load_tool", "mcp:docs:search"))
	settings.MaxDepth = 3
	mcp := NewSubagentMCP(ownerHooks, "")
	if _, err := newSubagentTool(settings, withExecutors(t, readOnlyTool("read")), map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, &fakeLauncher{}, mcp, nil); err != nil {
		t.Fatal(err)
	}
	copies := NewMCPCopies(session, map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{toolsetSearch}, Hooks: toolsetHooks}}, set, ownerHooks, "")
	bind := func() {
		if err := mcp.BindCopies(copies); err != nil {
			t.Fatal(err)
		}
	}
	if bindFirst {
		bind()
	}
	if err := mcp.SetLazy(set, &loadTool); err != nil {
		t.Fatal(err)
	}
	if !bindFirst {
		bind()
	}
	return copiesFixture{mcp: mcp, copies: copies, ownerCalls: &ownerCalls, toolsetCalls: &toolsetCalls, ownerMarker: ownerMarker, toolsetMarker: toolsetMarker}
}

func (f copiesFixture) agent(t *testing.T, name string) subagentAgent {
	t.Helper()
	for _, agent := range f.mcp.tool.agents {
		if agent.config.Name == name {
			return agent
		}
	}
	t.Fatalf("no agent %s", name)
	return subagentAgent{}
}

// The owner's catalog and every child catalog of a build hold one copy of each MCP tool: a session
// tool's copy is the owner's own copy (gated and hooked once), and the agents of one toolset share
// that toolset's copy, which stays apart from the owner's copy of the same ID. Every catalog variant
// of an agent (its own, the deepest level's, a sandbox choice's) holds it.
func TestMCPCopiesAreSharedByTheOwnerAndEveryChild(t *testing.T) {
	f := newCopiesFixture(t, false)
	session := f.copies.Session()
	if len(session) != 2 || session[0].Definition.ID != "mcp:docs:search" || session[1].Definition.ID != "mcp:docs:look" {
		t.Fatalf("session copies = %v", session)
	}
	if !session[0].Lazy || session[1].Lazy {
		t.Fatalf("owner copies: search lazy %v (want true), look lazy %v (want false)", session[0].Lazy, session[1].Lazy)
	}
	toolsetCopy := f.copies.toolsets[searchToolset][0]
	if closureOf(toolsetCopy) == closureOf(session[0]) {
		t.Fatal("the toolset's copy of mcp:docs:search is the owner's copy")
	}
	for name, want := range map[string][]tools.Tool{"s1": session, "s2": session, "t1": {toolsetCopy}, "t2": {toolsetCopy}} {
		agent := f.agent(t, name)
		if len(agent.mcpTools) != len(want) {
			t.Fatalf("%s holds %d MCP tools, want %d", name, len(agent.mcpTools), len(want))
		}
		for index, tool := range agent.mcpTools {
			if tool.Definition.ID != want[index].Definition.ID || closureOf(tool) != closureOf(want[index]) {
				t.Errorf("%s's %s is not the build's copy", name, tool.Definition.ID)
			}
		}
		for _, variant := range []struct {
			choice string
			leaf   bool
		}{{"", false}, {"", true}, {"reader", false}} {
			catalog, err := f.mcp.tool.childCatalog(agent, variant.choice, variant.leaf)
			if err != nil || catalog == nil {
				t.Fatalf("%s %+v: catalog %v, err %v", name, variant, catalog, err)
			}
			refused := executeNamedCall(t, catalog, "mcp_docs_search_0123456789", `{}`)
			if !strings.Contains(refused, "lazy_tool_not_loaded") {
				t.Errorf("%s %+v: a direct call to the lazy tool = %s, want lazy_tool_not_loaded", name, variant, refused)
			}
		}
	}
	// Six refused calls on each side: the owner's hooks fired for s1 and s2 (once per call, so the
	// copy was not hooked twice), the toolset's for t1 and t2, and no server ran.
	if *f.ownerCalls+*f.toolsetCalls != 0 || countLines(t, f.ownerMarker) != 6 || countLines(t, f.toolsetMarker) != 6 {
		t.Errorf("owner ran %d, toolset ran %d, owner hooks fired %d (want 6), toolset hooks %d (want 6)",
			*f.ownerCalls, *f.toolsetCalls, countLines(t, f.ownerMarker), countLines(t, f.toolsetMarker))
	}
	// A run through the child's load_tool reaches the agent's own connection once, through one gate.
	t1, err := f.mcp.tool.childCatalog(f.agent(t, "t1"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if ran := executeThroughLoadTool(t, t1, "mcp:docs:search", `{}`); !strings.Contains(ran, "toolset description ran") || *f.toolsetCalls != 1 || *f.ownerCalls != 0 {
		t.Errorf("t1's run = %s (toolset ran %d, owner ran %d), want the toolset's copy once", ran, *f.toolsetCalls, *f.ownerCalls)
	}
}

// The copies are gated before they are bound, so binding them before SetLazy gives the set leaves
// every child's lazy copy gated: the order of the two calls no longer decides it.
func TestMCPCopiesGateAChildWhateverTheBindOrder(t *testing.T) {
	f := newCopiesFixture(t, true)
	for _, name := range []string{"s1", "t1"} {
		catalog, err := f.mcp.tool.childCatalog(f.agent(t, name), "", false)
		if err != nil {
			t.Fatal(err)
		}
		if refused := executeNamedCall(t, catalog, "mcp_docs_search_0123456789", `{}`); !strings.Contains(refused, "lazy_tool_not_loaded") {
			t.Errorf("%s: a direct call to the lazy tool = %s, want lazy_tool_not_loaded", name, refused)
		}
		if got := strings.Join(toolNames(catalog), ","); !strings.Contains(got, "load_tool") {
			t.Errorf("%s: tools %s, want the owner's load_tool (SetLazy after the binding rebuilt the catalogs)", name, got)
		}
	}
	if *f.ownerCalls+*f.toolsetCalls != 0 {
		t.Errorf("a server ran: owner %d, toolset %d", *f.ownerCalls, *f.toolsetCalls)
	}
}

// NewMCPCopies gates with GateFor and never takes the set's one-shot Gate: copies built twice from
// one set are both gated (a toolset's copy of an owner's lazy ID too), and the set's Gate still
// gates the tool once and then refuses, as the owner's extension tools rely on.
func TestNewMCPCopiesLeavesTheOneShotGateToTheOwner(t *testing.T) {
	var calls int
	search := sameIDStub(t, "owner description", "ownerarg", &calls)
	set := tools.NewLazySet([]string{"mcp:docs:search"}, 256, nil)
	set.Bind([]tools.Tool{search})
	toolsets := map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{sameIDStub(t, "toolset description", "toolsetarg", &calls)}}}
	first := NewMCPCopies([]tools.Tool{search}, toolsets, set, sessionfs.HooksSettings{}, "")
	second := NewMCPCopies([]tools.Tool{search}, toolsets, set, sessionfs.HooksSettings{}, "")
	for index, copies := range []MCPCopies{first, second} {
		if !copies.Session()[0].Lazy || !copies.toolsets[searchToolset][0].Lazy {
			t.Errorf("build %d: session lazy %v, toolset lazy %v, want both gated", index, copies.Session()[0].Lazy, copies.toolsets[searchToolset][0].Lazy)
		}
	}
	if closureOf(first.Session()[0]) == closureOf(second.Session()[0]) {
		t.Error("two builds share one gate")
	}
	if gated, ok := set.Gate(search); !ok || !gated.Lazy {
		t.Fatalf("the set's Gate after the copies: ok %v, lazy %v, want a gate", ok, gated.Lazy)
	}
	if _, ok := set.Gate(search); ok {
		t.Fatal("the set's Gate gated the tool twice")
	}
	if none := NewMCPCopies([]tools.Tool{search}, nil, nil, sessionfs.HooksSettings{}, ""); none.Session()[0].Lazy || closureOf(none.Session()[0]) != closureOf(search) {
		t.Error("without a set or hooks the copy is not the listed tool itself")
	}
}
