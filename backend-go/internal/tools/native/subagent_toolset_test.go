package native

import (
	"context"
	"path/filepath"
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
	tool, err := newSubagentTool(settings, parentTools, map[string]subagentToolset{searchToolset: {native: withExecutors(t, toolset...)}}, launcher, nil, nil)
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

// A change tool in a toolset is in the child's set like any other tool, unless its own allow list
// leaves it out, and counts as a file changer.
func TestSubagentToolsetChangeToolFollowsTheToolsList(t *testing.T) {
	toolset := []tools.Tool{readOnlyTool("grep"), writeTool("exec")}
	// An allow list that names the change tool gives it.
	allowed := subagentSettings(toolsetAgent("fixer", "grep", "exec"))
	launcher := &fakeLauncher{}
	catalog, err := buildWithToolset(t, allowed, launcher, toolset, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "fixer"); got != "grep,exec" {
		t.Errorf("fixer tools = %q, want grep,exec", got)
	}
	open := subagentSettings(toolsetAgent("viewer"))
	launcher = &fakeLauncher{}
	if catalog, err = buildWithToolset(t, open, launcher, toolset, readOnlyTool("read")); err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := childToolsOf(launcher, "viewer"); got != "grep,exec" {
		t.Errorf("viewer tools = %q, want grep,exec: a child has every tool of its toolset", got)
	}
}

// A call to an agent whose change tool comes from a toolset counts as a file change, so it
// takes the checkpoint and runs in order; a read-only toolset agent does not.
func TestSubagentToolsetChangeToolCountsAsAFileChange(t *testing.T) {
	settings := subagentSettings(toolsetAgent("fixer", "exec"), toolsetAgent("viewer", "grep"))
	_, tool := buildSubagentCatalogWithToolset(t, settings, []tools.Tool{readOnlyTool("grep"), writeTool("exec")})
	if !tool.MayChangeFiles(map[string]any{"task": "x", "agent": "fixer"}) {
		t.Error("an agent whose exec comes from a toolset must count as a file changer")
	}
	if tool.MayChangeFiles(map[string]any{"task": "x", "agent": "viewer"}) {
		t.Error("a read-only toolset agent must not count as a file changer")
	}
}

// A toolset that lists the subagent tool does not give its copy to a child: the only subagent tool a
// child may have is the owner's nested copy (and with max_depth 1 there is none).
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
	tool, err := newSubagentTool(settings, withExecutors(t, parent...), map[string]subagentToolset{searchToolset: {native: withExecutors(t, toolset...)}}, &fakeLauncher{}, nil, nil)
	if err != nil {
		t.Fatalf("newSubagentTool: %v", err)
	}
	return nil, tool
}

// An agent with a toolset resolves its mcp: entries against that toolset's servers only.
func TestSubagentToolsetResolvesMCPEntriesAgainstItsOwnServers(t *testing.T) {
	sessionMCP := []tools.Tool{mcpStub(t, "browser", "click")}
	toolsetMCP := map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{mcpStub(t, "zvec", "search")}}}
	build := func(settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher) (*tools.Catalog, error) {
		hook := &SubagentMCP{}
		parentTools := withExecutors(t, readOnlyTool("read"))
		tool, err := newSubagentTool(settings, parentTools, map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, launcher, hook, nil)
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
		map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, &fakeLauncher{}, hook, nil); err != nil {
		t.Fatal(err)
	}
	err := hook.BindToolsets([]tools.Tool{mcpStub(t, "zvec", "search")}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a loaded MCP tool of toolset extra/search (loaded: none)") {
		t.Fatalf("error = %v", err)
	}
}

// sameIDStub is an MCP tool "mcp:docs:search" with its own description, schema and call counter, so
// two copies of one ID can be told apart.
func sameIDStub(t *testing.T, description, property string, calls *int) tools.Tool {
	t.Helper()
	schema := []byte(`{"type":"object","properties":{"` + property + `":{"type":"string"}}}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	tool := mcpStub(t, "docs", "search")
	tool.Definition.Description, tool.Definition.InputSchema = description, schema
	tool.Validator = validator
	tool.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
		*calls++
		return description + " ran", nil
	}
	return tool
}

// A toolset child whose MCP server has the ID of one of the owner's lazy servers keeps the toolset's
// copy. The owner's lazy list (the `load_tool` config) gates that copy, and load_tool shows the owner's description and schema,
// but a run executes the toolset's connection, and a direct call (refused) and a run each fire the
// toolset's hooks once and the owner's never.
func TestToolsetChildKeepsItsOwnCopyOfALazyOwnerServer(t *testing.T) {
	var ownerCalls, toolsetCalls int
	ownerTool := sameIDStub(t, "owner description", "ownerarg", &ownerCalls)
	toolsetTool := sameIDStub(t, "toolset description", "toolsetarg", &toolsetCalls)
	dir := t.TempDir()
	ownerMarker, toolsetMarker := filepath.Join(dir, "owner"), filepath.Join(dir, "toolset")
	ownerHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("owner", "*", "echo fired >> "+ownerMarker)}, nil)
	toolsetHooks := eventSettings([]sessionfs.ToolEventHook{eventHook("toolset", "*", "echo fired >> "+toolsetMarker)}, nil)

	set := tools.NewLazySet([]string{"mcp:docs:search"}, 256, nil)
	set.Bind([]tools.Tool{ownerTool})
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	mcp := NewSubagentMCP(ownerHooks, "")
	launcher := &fakeLauncher{}
	parentTools := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentTool(subagentSettings(toolsetAgent("finder", "grep", "load_tool", "mcp:docs:search")), parentTools,
		map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, launcher, mcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.SetLazy(set, &loadTool); err != nil {
		t.Fatal(err)
	}
	if err := mcp.BindToolsets([]tools.Tool{ownerTool}, map[string]ToolsetMCP{searchToolset: {Tools: []tools.Tool{toolsetTool}, Hooks: toolsetHooks}}); err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"finder"}`)
	child := launcher.requests[0].Tools

	described := executeNamedCall(t, child, "load_tool", `{"names":["mcp:docs:search"]}`)
	if !strings.Contains(described, "owner description") || !strings.Contains(described, "ownerarg") || strings.Contains(described, "toolset") {
		t.Errorf("describe = %s, want the owner's description and schema only", described)
	}

	refused := executeNamedCall(t, child, "mcp_docs_search_0123456789", `{}`)
	if !strings.Contains(refused, "lazy_tool_not_loaded") {
		t.Errorf("a direct call to the lazy toolset tool = %s, want lazy_tool_not_loaded", refused)
	}
	if ownerCalls+toolsetCalls != 0 || countLines(t, toolsetMarker) != 1 || countLines(t, ownerMarker) != 0 {
		t.Errorf("after the refusal: owner ran %d, toolset ran %d, toolset hook fired %d (want 1), owner hook fired %d (want 0)",
			ownerCalls, toolsetCalls, countLines(t, toolsetMarker), countLines(t, ownerMarker))
	}

	ran := executeThroughLoadTool(t, child, "mcp:docs:search", `{}`)
	if !strings.Contains(ran, "toolset description ran") {
		t.Errorf("run = %s, want the toolset copy's output", ran)
	}
	if ownerCalls != 0 || toolsetCalls != 1 || countLines(t, toolsetMarker) != 2 || countLines(t, ownerMarker) != 0 {
		t.Errorf("after the run: owner ran %d (want 0), toolset ran %d (want 1), toolset hook fired %d (want 2), owner hook fired %d (want 0)",
			ownerCalls, toolsetCalls, countLines(t, toolsetMarker), countLines(t, ownerMarker))
	}
}
