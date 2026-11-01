package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// A load_tool in the parent's pool is never a child's tool by itself: it reaches a child only when
// the child's agent names it and the owning profile set one with SetLazy.
func TestChildLoadToolComesOnlyFromTheOwnerAndOnlyWhenNamed(t *testing.T) {
	loadTool := readOnlyTool("load_tool")
	// The pool holds load_tool, as the parent's native tools do, but no child reads it from there.
	if readOnly, _ := childToolPools(subagentSettings(), []tools.Tool{readOnlyTool("read"), loadTool}); !containsTool(readOnly, "load_tool") {
		t.Fatalf("childToolPools dropped load_tool from the pool it reads: %v", readOnly)
	}
	named := agentNamed("explore")
	named.Tools, named.HasTools = []string{"read", "load_tool"}, true
	unnamed := agentNamed("plain")
	unnamed.Tools, unnamed.HasTools = []string{"read"}, true

	// No owner set: a child that names load_tool gets none.
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(named), launcher, readOnlyTool("read"), loadTool)
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := toolNames(launcher.requests[0].Tools); strings.Join(got, ",") != "read" {
		t.Fatalf("child with no owning load_tool got %v, want read only", got)
	}

	// The owner gives its load_tool: the child that names it gets exactly one, after read.
	launcher = &fakeLauncher{}
	hook := &SubagentMCP{}
	parentTools := withExecutors(t, readOnlyTool("read"), loadTool)
	tool, err := NewSubagentToolWithMCP(subagentSettings(named, unnamed), parentTools, launcher, hook)
	if err != nil {
		t.Fatalf("NewSubagentToolWithMCP: %v", err)
	}
	// The owner's load_tool is a real, validated tool, as BuildCatalogWithAsync builds it.
	owned := withExecutors(t, loadTool)[0]
	hook.SetLazy(nil, &owned)
	catalog, err = tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if got := toolNames(launcher.requests[0].Tools); strings.Join(got, ",") != "read,load_tool" {
		t.Fatalf("child that names load_tool got %v, want read,load_tool", got)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"plain"}`)
	if got := toolNames(launcher.requests[1].Tools); strings.Join(got, ",") != "read" {
		t.Fatalf("child that does not name load_tool got %v, want read only", got)
	}
	// The parent's order is unchanged: load_tool is in the parent's own pool, not added after.
	if strings.Join(toolNames(catalog), ",") != "read,load_tool,subagent" {
		t.Fatalf("parent catalog changed: %v", toolNames(catalog))
	}
}

func containsTool(list []tools.Tool, name string) bool {
	for _, tool := range list {
		if tool.Definition.Name == name {
			return true
		}
	}
	return false
}

// An agent with no tools list gets every read-only tool of the pool it is built from. A load_tool in
// that pool must not reach it: the owner's load_tool is offered only to an agent that names it.
func TestAgentWithoutAToolsListGetsNoLoadToolFromTheParentPool(t *testing.T) {
	loadTool := readOnlyTool("load_tool")
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"), loadTool)
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := toolNames(launcher.requests[0].Tools); strings.Join(got, ",") != "read" {
		t.Fatalf("child with no tools list got %v, want read only (load_tool came from the parent pool)", got)
	}
}
