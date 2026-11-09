package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// A load_tool in the parent's pool is never a child's tool by itself: it reaches a child only from the
// owning profile (SetLazy), and then by default, like a profile's own, unless the agent's allow list
// leaves it out.
func TestChildLoadToolComesOnlyFromTheOwnerAndByDefault(t *testing.T) {
	loadTool := readOnlyTool("load_tool")
	loadTool.Definition.ID = loadToolID
	// The pool holds load_tool, as a hand-built parent list may, but no child reads it from there.
	if pool := childNativePool([]tools.Tool{readOnlyTool("read"), loadTool}); containsTool(pool, "load_tool") {
		t.Fatalf("childNativePool kept load_tool, which only the owner hands out: %v", pool)
	}
	plain := agentNamed("plain")
	narrowed := agentNamed("narrowed")
	narrowed.Tools, narrowed.HasTools = []string{"read"}, true

	// No owner set: no child gets one.
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(plain), launcher, readOnlyTool("read"), loadTool)
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := toolNames(launcher.requests[0].Tools); strings.Join(got, ",") != "read" {
		t.Fatalf("child with no owning load_tool got %v, want read only", got)
	}

	// The owner gives its load_tool: a child gets exactly one, unless its allow list leaves it out.
	launcher = &fakeLauncher{}
	hook := &SubagentMCP{}
	parentTools := withExecutors(t, readOnlyTool("read"), loadTool)
	tool, err := NewSubagentToolWithMCP(subagentSettings(plain, narrowed), parentTools, launcher, hook)
	if err != nil {
		t.Fatalf("NewSubagentToolWithMCP: %v", err)
	}
	// The owner's load_tool is a real, validated tool, as BuildCatalogWithAsync builds it.
	owned := withExecutors(t, loadTool)[0]
	if err := hook.SetLazy(nil, &owned); err != nil {
		t.Fatal(err)
	}
	catalog, err = tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"plain"}`)
	if got := toolNames(launcher.requests[0].Tools); strings.Join(got, ",") != "read,load_tool" {
		t.Fatalf("child with the owner's load_tool got %v, want read,load_tool", got)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"narrowed"}`)
	if got := toolNames(launcher.requests[1].Tools); strings.Join(got, ",") != "read" {
		t.Fatalf("child whose allow list is read got %v, want read only", got)
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

// SetLazy builds the catalogs again from the same parts, so a second call leaves one load_tool and
// not a duplicate.
func TestSetLazyTwiceLeavesOneLoadTool(t *testing.T) {
	parentTools := withExecutors(t, readOnlyTool("read"))
	hook := &SubagentMCP{}
	if _, err := NewSubagentToolWithMCP(subagentSettings(agentNamed("explore")), parentTools, &fakeLauncher{}, hook); err != nil {
		t.Fatalf("NewSubagentToolWithMCP: %v", err)
	}
	owned := withExecutors(t, readOnlyTool("load_tool"))[0]
	for range 2 {
		if err := hook.SetLazy(nil, &owned); err != nil {
			t.Fatalf("SetLazy: %v", err)
		}
	}
	if got := strings.Join(toolNames(hook.tool.agents[0].catalog), ","); got != "read,load_tool" {
		t.Fatalf("child tools = %q, want read,load_tool", got)
	}
}
