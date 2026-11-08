package app

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChildOfAParentWithLazyToolsRunsThemThroughItsLoadToolInTheOwningProfilesSet(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{"load_tool", `{"run":"mcp:fixture:lookup","arguments":{"word":"answer"}}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests, want the run and the answer", len(child))
	}
	if !slices.Contains(child[0].Tools, "load_tool") {
		t.Fatalf("the child's load_tool was absent from its first request: %v", child[0].Tools)
	}
	requireStableTools(t, child, name)
	// The child's load_tool dispatched through the child's own catalog: the MCP server ran once.
	if calls.Load() != 1 || !strings.Contains(child[len(child)-1].Text, "MCP says answer is 99") {
		t.Errorf("the child's run reached the MCP server %d times; history:\n%s", calls.Load(), child[len(child)-1].Text)
	}
	// Nothing was written to the owning profile's set, and the caller's is untouched.
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 0 {
		t.Errorf("the owning profile's lazy_tools.json holds %v, want no loads", loaded)
	}
}

// An owner with no tools.lazy gives its children no load_tool, even when the agent names it: load_tool
// is offered only through the owner's set. The child's request lists no load_tool.
func TestChildOfAParentWithoutLazyToolsGetsNoLoadTool(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "load_tool"}, "__load_tool": true}, false, nil, nil)
	f.provider.childFirstCall = [2]string{"skill", notesCall}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 1 {
		t.Fatal("child made no request")
	}
	if slices.Contains(child[0].Tools, "load_tool") {
		t.Fatalf("a child of a profile with no tools.lazy was sent load_tool: %v", child[0].Tools)
	}
}
