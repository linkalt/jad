package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

const mergedToolsetKey = "a/x+b/y"

func TestSubagentTwoToolsetsUseTheCompositeKey(t *testing.T) {
	agent := toolsetAgent("finder")
	agent.Toolset = []string{"a/x", "b/y"}
	launcher := &fakeLauncher{}
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentTool(subagentSettings(agent), parent,
		map[string]subagentToolset{mergedToolsetKey: {native: withExecutors(t, readOnlyTool("grep"), readOnlyTool("glob"))}}, launcher, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "finder"); got != "grep,glob" {
		t.Errorf("finder tools = %q, want the merged grep,glob", got)
	}

	_, err = newSubagentTool(subagentSettings(agent), parent,
		map[string]subagentToolset{"a/x": {native: withExecutors(t, readOnlyTool("grep"))}}, &fakeLauncher{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `agents.finder.toolset "a/x+b/y" was not loaded`) {
		t.Fatalf("error = %v", err)
	}
}
