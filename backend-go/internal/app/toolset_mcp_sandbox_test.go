package app

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// A toolset's MCP servers start in the toolset's sandbox for the agents without one of their
// own, and once more, in its sandbox, for each agent whose own sandbox covers mcp.
func TestToolsetMCPServersStartInTheAgentSandbox(t *testing.T) {
	session := &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", Tools: []string{"exec", "mcp"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{tmp}"}}
	reader := &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", Tools: []string{"exec", "mcp"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{tmp}"}}
	execOnly := &sessionfs.SandboxSettings{Backend: "bwrap", Executable: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{tmp}"}}
	agent := func(name string, sandbox *sessionfs.SandboxSettings) sessionfs.NativeSubagentAgent {
		return sessionfs.NativeSubagentAgent{Name: name, Toolset: []string{"extra/search"}, Sandbox: sandbox}
	}
	config := sessionfs.SessionConfig{
		WorkingDir:     "/work",
		AgentToolsets:  map[string]sessionfs.SessionConfig{"extra/search": {WorkingDir: "/work", Sandbox: session}},
		NativeSubagent: &sessionfs.NativeSubagentSettings{Agents: []sessionfs.NativeSubagentAgent{agent("a", nil), agent("b", nil), agent("boxed", reader), agent("exec-only", execOnly), {Name: "plain"}}},
	}
	uses := toolsetMCPUses(config)
	if len(uses) != 2 {
		t.Fatalf("uses = %+v", uses)
	}
	byKey := map[string]toolsetMCPUse{}
	for _, use := range uses {
		byKey[use.key] = use
	}
	shared, boxed := byKey["extra/search"], byKey[native.ToolsetMCPKey(agent("boxed", reader))]
	if shared.config.Sandbox != session || boxed.config.Sandbox != reader || boxed.toolset != "extra/search" {
		t.Fatalf("shared %+v, boxed %+v", shared.config.Sandbox, boxed.config.Sandbox)
	}
	if config.AgentToolsets["extra/search"].Sandbox != session {
		t.Fatal("the toolset's own settings changed")
	}
	key, wrap := mcpSandboxWrapper(boxed.config)
	if wrap == nil || !strings.Contains(key, `"{tmp}"`) || strings.Contains(key, `"{workspace}"`) {
		t.Fatalf("the boxed agent's servers are not wrapped in its sandbox: %q", key)
	}
	if toolsetMCPUses(sessionfs.SessionConfig{NativeSubagent: config.NativeSubagent}) != nil {
		t.Fatal("no toolsets, yet uses")
	}
}
