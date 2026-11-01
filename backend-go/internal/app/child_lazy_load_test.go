package app

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A child of a profile with tools.lazy gets its lazy MCP tool gated with the owning profile's set:
// absent from the child's first request, present from the next one after the child loads it, and
// the load is written to the owning profile's lazy_tools.json. The caller's state is untouched.
func TestChildOfAParentWithLazyToolsLoadsIntoTheOwningProfilesSet(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "read", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"read", "load_tool", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{"load_tool", `{"names":["` + name + `"]}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests, want the load and the answer", len(child))
	}
	if slices.Contains(child[0].Tools, name) {
		t.Fatalf("the lazy MCP tool was in the child's first request: %v", child[0].Tools)
	}
	if !slices.Contains(child[0].Tools, "load_tool") {
		t.Fatalf("the child's load_tool was absent from its first request: %v", child[0].Tools)
	}
	if !slices.Contains(child[1].Tools, name) {
		t.Fatalf("the loaded MCP tool was absent from the child's next request: %v", child[1].Tools)
	}
	// The owning profile's set holds the load; the caller of the remote path is not involved here.
	if loaded := lazyFileLoaded(t, f.lazyFile()); !slices.Contains(loaded, "mcp:fixture:lookup") {
		t.Errorf("the owning profile's lazy_tools.json holds %v, want mcp:fixture:lookup", loaded)
	}
}

// An owner with no tools.lazy gives its children no load_tool, even when the agent names it: load_tool
// is offered only through the owner's set. The child's request lists no load_tool.
func TestChildOfAParentWithoutLazyToolsGetsNoLoadTool(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"read", "load_tool"}, "__load_tool": true}, false, nil, nil)
	f.provider.childFirstCall = [2]string{"read", `{"path":"notes.txt"}`}
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
