package native

import (
	"context"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const searchToolset = "extra/search"

func toolsetAgent(name string, names ...string) sessionfs.NativeSubagentAgent {
	agent := agentNamed(name)
	agent.Toolset = []string{searchToolset}
	if len(names) > 0 {
		agent.Tools, agent.HasTools = names, true
	}
	return agent
}

// buildWithToolset builds the subagent tool over the session's parent tools and one
// toolset, the way the catalog builder does.
func buildWithToolset(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, toolset []tools.Tool, parent ...tools.Tool) (*tools.Catalog, error) {
	t.Helper()
	parentTools := withExecutors(t, parent...)
	tool, err := newSubagentTool(settings, parentTools, map[string]subagentToolset{searchToolset: {native: withExecutors(t, toolset...)}}, launcher, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, nil
}

func childToolsOf(launcher *fakeLauncher, agent string) string {
	for _, request := range launcher.requests {
		if request.Agent.Name == agent {
			return strings.Join(toolNames(request.Tools), ",")
		}
	}
	return "<no run>"
}

// A tool the session does not register reaches a child through its toolset, and the
// parent's catalog does not gain it. An agent with no toolset keeps the session's tools.
func TestSubagentToolsetGivesTheChildToolsTheSessionDoesNotRegister(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, err := buildWithToolset(t, subagentSettings(toolsetAgent("finder"), agentNamed("plain")), launcher,
		[]tools.Tool{readOnlyTool("grep"), readOnlyTool("glob")}, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"finder"}`)
	subagentCall(t, catalog, `{"task":"x","agent":"plain"}`)
	if got := childToolsOf(launcher, "finder"); got != "grep,glob" {
		t.Errorf("finder tools = %q, want the toolset's grep,glob and not the session's read", got)
	}
	if got := childToolsOf(launcher, "plain"); got != "read" {
		t.Errorf("plain tools = %q, want the session's read", got)
	}
	if got := strings.Join(toolNames(catalog), ","); got != "read,subagent" {
		t.Errorf("parent catalog = %q, want read,subagent: a toolset's tools are not the parent's", got)
	}
}

// tools narrows within the toolset, and a name only the session registers is refused with
// the toolset named in the error.
func TestSubagentToolsetToolsListNarrowsWithinTheToolset(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, err := buildWithToolset(t, subagentSettings(toolsetAgent("finder", "glob")), launcher,
		[]tools.Tool{readOnlyTool("grep"), readOnlyTool("glob")}, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "finder"); got != "glob" {
		t.Errorf("finder tools = %q, want glob", got)
	}
	_, err = buildWithToolset(t, subagentSettings(toolsetAgent("finder", "read")), &fakeLauncher{},
		[]tools.Tool{readOnlyTool("grep")}, readOnlyTool("read"))
	if err == nil || !strings.Contains(err.Error(), `lists "read", which is not an enabled native tool (enabled: grep)`) || !strings.Contains(err.Error(), "its tools come from toolset "+searchToolset) {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentToolsetThatWasNotBuiltIsAnError(t *testing.T) {
	_, err := NewSubagentTool(subagentSettings(toolsetAgent("finder")), withExecutors(t, readOnlyTool("read")), nil)
	if err == nil || !strings.Contains(err.Error(), `agents.finder.toolset "extra/search" was not loaded`) {
		t.Fatalf("error = %v", err)
	}
}

// The profile's tools.deny removes a tool from the toolset's children too.
func TestSubagentToolsetIsStillSubjectToProfileDeny(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, err := buildWithToolset(t, withDeny(subagentSettings(toolsetAgent("finder")), "glob"), launcher,
		[]tools.Tool{readOnlyTool("grep"), readOnlyTool("glob")}, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "finder"); got != "grep" {
		t.Errorf("finder tools = %q, want grep: glob is denied", got)
	}
	// An explicit entry the deny covers is dropped, not an error, as for the session's tools.
	if _, err := buildWithToolset(t, withDeny(subagentSettings(toolsetAgent("finder", "grep", "glob")), "glob"), &fakeLauncher{},
		[]tools.Tool{readOnlyTool("grep"), readOnlyTool("glob")}, readOnlyTool("read")); err != nil {
		t.Fatalf("a denied entry must be dropped: %v", err)
	}
}

// A change tool in a toolset is given by the agent's tools list alone, and counts as a file
// changer.
func TestSubagentToolsetChangeToolFollowsTheToolsList(t *testing.T) {
	toolset := []tools.Tool{readOnlyTool("grep"), writeTool("write")}
	// With no tools list the change tool is never given.
	allowed := subagentSettings(toolsetAgent("fixer", "grep", "write"))
	launcher := &fakeLauncher{}
	catalog, err := buildWithToolset(t, allowed, launcher, toolset, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "fixer"); got != "grep,write" {
		t.Errorf("fixer tools = %q, want grep,write", got)
	}
	open := subagentSettings(toolsetAgent("viewer"))
	launcher = &fakeLauncher{}
	if catalog, err = buildWithToolset(t, open, launcher, toolset, readOnlyTool("read")); err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "viewer"); got != "grep" {
		t.Errorf("viewer tools = %q, want grep only: change tools must be named", got)
	}
}

// A call to an agent whose change tool comes from a toolset counts as a file change, so it
// takes the checkpoint and runs in order; a read-only toolset agent does not.
func TestSubagentToolsetChangeToolCountsAsAFileChange(t *testing.T) {
	settings := subagentSettings(toolsetAgent("fixer", "write"), toolsetAgent("viewer", "grep"))
	_, tool := buildSubagentCatalogWithToolset(t, settings, []tools.Tool{readOnlyTool("grep"), writeTool("write")})
	if !tool.MayChangeFiles(map[string]any{"task": "x", "agent": "fixer"}) {
		t.Error("an agent whose write comes from a toolset must count as a file changer")
	}
	if tool.MayChangeFiles(map[string]any{"task": "x", "agent": "viewer"}) {
		t.Error("a read-only toolset agent must not count as a file changer")
	}
}

// A toolset that lists the subagent tool does not give it to a child by itself: like async and
// tool_smith it is opt-in through the agent's own tools list.
func TestSubagentToolsetNeverGivesTheChildTheSubagentTool(t *testing.T) {
	launcher := &fakeLauncher{}
	nested := tools.Tool{Definition: tools.Definition{ID: subagentToolID, Source: "native", Name: "subagent", Description: "x"}, ReadOnly: true}
	catalog, err := buildWithToolset(t, subagentSettings(toolsetAgent("finder")), launcher,
		[]tools.Tool{readOnlyTool("grep"), nested}, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "finder"); got != "grep" {
		t.Errorf("finder tools = %q, want grep: a child must not be able to start another child", got)
	}
}

// buildSubagentCatalogWithToolset returns the subagent tool built over one toolset.
func buildSubagentCatalogWithToolset(t *testing.T, settings sessionfs.NativeSubagentSettings, toolset []tools.Tool, parent ...tools.Tool) (*tools.Catalog, tools.Tool) {
	t.Helper()
	tool, err := newSubagentTool(settings, withExecutors(t, parent...), map[string]subagentToolset{searchToolset: {native: withExecutors(t, toolset...)}}, &fakeLauncher{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("newSubagentTool: %v", err)
	}
	return nil, tool
}

// A child that gets process from its toolset starts its processes in the toolset's
// process manager, and its run's end stops them there. The session's manager and its
// processes are not touched.
func TestSubagentToolsetProcessesAreStoppedInTheToolsetsManager(t *testing.T) {
	session := newProcessFixture(t)
	toolset := newProcessFixture(t)
	sessionProcess := session.start(t, "", "sleep 30")

	agent := writerAgent("runner", "process")
	agent.Toolset = []string{searchToolset}
	process := toolset.tool
	process.ReadOnly = false
	settings := writeSettings(agent)

	var started string
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		started = toolset.start(t, request.Execution.ProcessOwner, "sleep 30")
		return SubagentResult{Text: "done", Rounds: 1, Tokens: 1}, nil
	}}
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentTool(settings, parent, map[string]subagentToolset{searchToolset: {native: []tools.Tool{process}, processes: toolset.manager}}, launcher, nil, manager, session.manager)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parent, tool))
	if err != nil {
		t.Fatal(err)
	}

	out := subagentCall(t, catalog, `{"task":"serve"}`)
	if started == "" {
		t.Fatalf("the child never ran:\n%s", out)
	}
	if got := toolset.state(t, started); got != ProcessStopped {
		t.Errorf("the child's process is %s after its run ended, want stopped", got)
	}
	if got := session.state(t, sessionProcess); got != ProcessRunning {
		t.Errorf("the session's own process is %s: a child's end must not touch it", got)
	}
	if !strings.Contains(out, "stopped_processes: 1") {
		t.Errorf("the result does not say a process was stopped:\n%s", out)
	}
}

// An agent with a toolset resolves its mcp: entries against that toolset's servers only.
func TestSubagentToolsetResolvesMCPEntriesAgainstItsOwnServers(t *testing.T) {
	sessionMCP := []tools.Tool{mcpStub(t, "browser", "click")}
	toolsetMCP := map[string][]tools.Tool{searchToolset: {mcpStub(t, "zvec", "search")}}
	build := func(settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher) (*tools.Catalog, error) {
		hook := &SubagentMCP{}
		parentTools := withExecutors(t, readOnlyTool("read"))
		tool, err := newSubagentTool(settings, parentTools, map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, launcher, hook, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := hook.BindToolsets(sessionMCP, toolsetMCP); err != nil {
			return nil, err
		}
		return tools.NewCatalog(append(parentTools, tool))
	}

	launcher := &fakeLauncher{}
	catalog, err := build(subagentSettings(toolsetAgent("finder", "grep", "mcp:zvec:search"), agentWithTools("plain", "read", "mcp:browser:click")), launcher)
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"finder"}`)
	subagentCall(t, catalog, `{"task":"x","agent":"plain"}`)
	if got := childToolsOf(launcher, "finder"); got != "grep,mcp_zvec_search_0123456789" {
		t.Errorf("finder tools = %q", got)
	}
	if got := childToolsOf(launcher, "plain"); got != "read,mcp_browser_click_0123456789" {
		t.Errorf("plain tools = %q", got)
	}

	for name, tc := range map[string]struct {
		agent sessionfs.NativeSubagentAgent
		want  string
	}{
		"the session's server is not the toolset's": {toolsetAgent("finder", "mcp:browser:click"), `"mcp:browser:click", which is not a loaded MCP tool of toolset extra/search (loaded: mcp:zvec:search)`},
		"the toolset's server is not the session's": {agentWithTools("plain", "mcp:zvec:search"), `"mcp:zvec:search", which is not a loaded MCP tool (loaded: mcp:browser:click)`},
		"wildcard of the other side":                {toolsetAgent("finder", "mcp:browser:*"), `server "browser" has no loaded tool`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := build(subagentSettings(tc.agent), &fakeLauncher{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A toolset that loaded no MCP tool at all says so instead of failing on a nil map.
func TestSubagentToolsetWithNoLoadedMCPToolsNamesNone(t *testing.T) {
	hook := &SubagentMCP{}
	parentTools := withExecutors(t, readOnlyTool("read"))
	if _, err := newSubagentTool(subagentSettings(toolsetAgent("finder", "grep", "mcp:zvec:search")), parentTools,
		map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, &fakeLauncher{}, hook, nil, nil); err != nil {
		t.Fatal(err)
	}
	err := hook.BindToolsets([]tools.Tool{mcpStub(t, "zvec", "search")}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a loaded MCP tool of toolset extra/search (loaded: none)") {
		t.Fatalf("error = %v", err)
	}
}
