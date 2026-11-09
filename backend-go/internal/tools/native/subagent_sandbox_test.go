package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// childDescriptionOf is the description of tool id in the catalog the launcher got for agent.
func childDescriptionOf(t *testing.T, launcher *fakeLauncher, agent, id string) string {
	t.Helper()
	for _, request := range launcher.requests {
		if request.Agent.Name != agent {
			continue
		}
		for _, definition := range request.Tools.Definitions() {
			if definition.ID == id {
				return definition.Description
			}
		}
		t.Fatalf("agent %s has no tool %s", agent, id)
	}
	t.Fatalf("agent %s did not run", agent)
	return ""
}

// An agent with a sandbox of its own reads that sandbox in its exec and tool_smith
// descriptions, from the session's tools and from its toolset's; an agent without one keeps
// the session's text, and the session's tools are not changed.
func TestAgentSandboxDescribesTheChildTools(t *testing.T) {
	session := bwrapSandbox()
	reader := bwrapSandbox()
	reader.WritablePaths, reader.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	noExec := bwrapSandbox()
	noExec.Tools = []string{"bash", "mcp"}

	stub := func(id, name, base string, sandbox *sessionfs.SandboxSettings) tools.Tool {
		return tools.Tool{Definition: tools.Definition{ID: id, Source: "native", Name: name, Description: base + sandboxDescription(sandbox)}}
	}
	parent := withExecutors(t, stub(execToolID, "exec", "session exec.", session), stub(toolSmithToolID, "tool_smith", "session smith.", session), readOnlyTool("read"))
	toolset := withExecutors(t, stub(execToolID, "exec", "toolset exec.", session), readOnlyTool("grep"))
	bases := map[string]string{execToolID: "session exec.", toolSmithToolID: "session smith."}

	boxed, uncovered, plain := agentNamed("boxed"), agentNamed("uncovered"), agentNamed("plain")
	boxed.Sandbox, uncovered.Sandbox = reader, noExec
	tooled := toolsetAgent("tooled")
	tooled.Sandbox = reader
	launcher := &fakeLauncher{}
	tool, err := newSubagentToolDescribed(subagentSettings(boxed, uncovered, plain, tooled), parent, bases,
		map[string]subagentToolset{searchToolset: {native: toolset, descriptions: map[string]string{execToolID: "toolset exec."}}}, launcher, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append([]tools.Tool(nil), parent...), tool))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boxed", "uncovered", "plain", "tooled"} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+name+`"}`)
	}
	for _, check := range []struct{ agent, id, want string }{
		{"boxed", execToolID, "session exec." + sandboxDescription(reader)},
		{"boxed", toolSmithToolID, "session smith." + sandboxDescription(reader)},
		{"uncovered", execToolID, "session exec."},
		{"plain", execToolID, "session exec." + sandboxDescription(session)},
		{"tooled", execToolID, "toolset exec." + sandboxDescription(reader)},
	} {
		if got := childDescriptionOf(t, launcher, check.agent, check.id); got != check.want {
			t.Errorf("%s %s description = %q, want %q", check.agent, check.id, got, check.want)
		}
	}
	if !strings.Contains(childDescriptionOf(t, launcher, "boxed", execToolID), "Writable: $TMPDIR") {
		t.Error("the reader's description does not name its writable paths")
	}
	if parent[0].Definition.Description != "session exec."+sandboxDescription(session) {
		t.Errorf("the session's exec changed: %q", parent[0].Definition.Description)
	}
}

// A toolset agent whose own sandbox covers mcp gets the toolset's servers started for it (its
// own ToolsetMCPKey); the toolset's other agents share the toolset's.
func TestAgentSandboxKeysItsToolsetServers(t *testing.T) {
	covering := bwrapSandbox()
	boxed := toolsetAgent("boxed")
	boxed.Sandbox = covering
	shared := toolsetAgent("shared")
	notMCP := toolsetAgent("not-mcp")
	notMCP.Sandbox = bwrapSandbox()
	notMCP.Sandbox.Tools = []string{"exec"}
	if ToolsetMCPKey(shared) != searchToolset || ToolsetMCPKey(notMCP) != searchToolset || ToolsetMCPKey(agentNamed("none")) != "" {
		t.Fatalf("keys: shared %q, not-mcp %q", ToolsetMCPKey(shared), ToolsetMCPKey(notMCP))
	}
	boxedKey := ToolsetMCPKey(boxed)
	if boxedKey == searchToolset || !strings.HasPrefix(boxedKey, searchToolset) {
		t.Fatalf("boxed key = %q", boxedKey)
	}

	hook := &SubagentMCP{}
	launcher := &fakeLauncher{}
	parentTools := withExecutors(t, readOnlyTool("read"))
	tool, err := newSubagentTool(subagentSettings(boxed, shared, notMCP), parentTools, map[string]subagentToolset{searchToolset: {native: withExecutors(t, readOnlyTool("grep"))}}, launcher, hook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.BindToolsets(nil, map[string]ToolsetMCP{
		searchToolset: {Tools: []tools.Tool{mcpStub(t, "zvec", "search")}},
		boxedKey:      {Tools: []tools.Tool{mcpStub(t, "zvecboxed", "search")}},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boxed", "shared", "not-mcp"} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+name+`"}`)
	}
	for agent, want := range map[string]string{"boxed": "grep,mcp_zvecboxed_search_0123456789", "shared": "grep,mcp_zvec_search_0123456789", "not-mcp": "grep,mcp_zvec_search_0123456789"} {
		if got := childToolsOf(launcher, agent); got != want {
			t.Errorf("%s tools = %q, want %q", agent, got, want)
		}
	}
}

// The catalog builder hands the subagent tool the base descriptions of the session's tools and
// of each toolset's, so a sandboxed agent reads its own sandbox through either.
func TestCatalogDescribesAnAgentSandbox(t *testing.T) {
	session := bwrapSandbox()
	reader := bwrapSandbox()
	reader.WritablePaths, reader.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	smith := sessionfs.NativeToolSmithSettings{}
	smith.Child.Sandbox = session
	manifests := []string{sampleToolSmithManifest, filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")}
	boxed, tooled := agentNamed("boxed"), toolsetAgent("tooled")
	boxed.Sandbox, tooled.Sandbox = reader, reader
	subagent := subagentSettings(boxed, tooled)
	subagent.StorageDir = t.TempDir()
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: manifests,
		NativeToolSmith:     &smith,
		NativeSubagent:      &subagent,
		AgentToolsets: map[string]sessionfs.SessionConfig{tooled.ToolsetKey(): {
			SettingPath: filepath.Join(t.TempDir(), "toolset.json"), ToolDefinitionPaths: manifests[:1], NativeToolSmith: &smith,
		}},
	}
	launcher := &fakeLauncher{}
	catalog, err := BuildCatalogWithSubagents(config, nil, nil, nil, launcher, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boxed", "tooled"} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+name+`"}`)
		got := childDescriptionOf(t, launcher, name, toolSmithToolID)
		if !strings.HasSuffix(got, sandboxDescription(reader)) || strings.Contains(got, sandboxDescription(session)) {
			t.Errorf("%s tool_smith description = %q", name, got)
		}
	}
}
