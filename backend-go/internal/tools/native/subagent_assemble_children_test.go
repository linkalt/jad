package native

import (
	"encoding/json"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// TestAssembleChildrenBuildsEachChildCatalogOnceFromEveryPart: a subagent tool made for a catalog
// assembly builds no child catalog when it is made; AssembleChildren builds each agent's own and
// deepest-level catalogs once, from the lazy set, the load_tool and the MCP copies together, and
// they equal, definition for definition, the catalogs the incremental build ends with.
func TestAssembleChildrenBuildsEachChildCatalogOnceFromEveryPart(t *testing.T) {
	assembled := newAssembledChildCatalogFixture(t, "/work")
	incremental := newChildCatalogFixture(t, "/work")
	if len(assembled.builtBefore) != 0 {
		t.Fatalf("agents with catalogs before AssembleChildren: %v", assembled.builtBefore)
	}
	tool, other := assembled.mcp.tool, incremental.mcp.tool
	if len(tool.agents) != len(other.agents) {
		t.Fatalf("agents: %d assembled, %d incremental", len(tool.agents), len(other.agents))
	}
	for index, agent := range tool.agents {
		if agent.catalogs == nil {
			t.Fatalf("%s: no catalogs after AssembleChildren", agent.config.Name)
		}
		want := 1
		if agent.nests {
			want = 2
		}
		agent.catalogs.mu.Lock()
		built := len(agent.catalogs.byKey)
		agent.catalogs.mu.Unlock()
		if built != want {
			t.Errorf("%s: %d catalogs built by the assembly, want %d (own and deepest level)", agent.config.Name, built, want)
		}
		for _, leaf := range []bool{false, true} {
			got, err := tool.childCatalog(agent, "", leaf)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := other.childCatalog(other.agents[index], "", leaf)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got.Definitions())
			wantJSON, _ := json.Marshal(expected.Definitions())
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("%s leaf=%v:\nassembled   %s\nincremental %s", agent.config.Name, leaf, gotJSON, wantJSON)
			}
		}
	}
	if tool.self.Definition.Description != other.self.Definition.Description {
		t.Errorf("subagent description differs:\nassembled   %q\nincremental %q", tool.self.Definition.Description, other.self.Definition.Description)
	}
}

// TestAssembleChildrenRefusesWhatTheIncrementalBuildRefuses: an agent naming an MCP tool no server
// listed fails the assembly with the incremental build's error text, and a nil handle or one
// without a subagent tool assembles nothing.
func TestAssembleChildrenRefusesWhatTheIncrementalBuildRefuses(t *testing.T) {
	agent := agentNamed("looker")
	agent.Tools, agent.HasTools = []string{"read", "mcp:srv:missing"}, true
	parentTools := withExecutors(t, readOnlyTool("read"))
	present := childCatalogMCP(t, "srv", "look")

	incremental := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	if _, err := NewSubagentToolWithMCP(subagentSettings(agent), parentTools, &fakeLauncher{}, incremental); err != nil {
		t.Fatal(err)
	}
	wantErr := incremental.BindToolsets([]tools.Tool{present}, nil)
	if wantErr == nil {
		t.Fatal("incremental build accepted a missing MCP tool")
	}
	assembled := NewAssembledSubagentMCP(sessionfs.HooksSettings{}, "")
	if _, err := NewSubagentToolWithMCP(subagentSettings(agent), parentTools, &fakeLauncher{}, assembled); err != nil {
		t.Fatal(err)
	}
	err := assembled.AssembleChildren(ChildParts{MCP: NewMCPCopies([]tools.Tool{present}, nil, nil, sessionfs.HooksSettings{}, "")})
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("AssembleChildren error = %v, want %v", err, wantErr)
	}

	var none *SubagentMCP
	if err := none.AssembleChildren(ChildParts{}); err != nil {
		t.Fatalf("nil handle: %v", err)
	}
	if err := NewAssembledSubagentMCP(sessionfs.HooksSettings{}, "").AssembleChildren(ChildParts{}); err != nil {
		t.Fatalf("handle without a tool: %v", err)
	}
}
