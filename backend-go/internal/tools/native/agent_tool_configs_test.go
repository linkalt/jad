package native

import (
	"reflect"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// The tool side of an owner run's agents is one derivation from the owner run's config: each
// toolset's native config is the loaded toolset with the owner's model and, without the owner's
// async section, no async; each MCP view is that same config, in the agent's sandbox when it
// covers mcp. Only those fields differ from the loaded toolset, and the owner's map is untouched.
func TestAgentToolConfigsAreOneDerivationFromTheOwnerRun(t *testing.T) {
	toolsetBox := &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"exec", "mcp"}, WritablePaths: []string{"{workspace}"}}
	agentBox := &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"mcp"}, WritablePaths: []string{"{tmp}"}}
	execBox := &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"exec"}}
	loaded := sessionfs.SessionConfig{
		WorkingDir: "/work", Sandbox: toolsetBox, NativeAsync: &sessionfs.NativeAsyncSettings{},
		MCPServers: []sessionfs.MCPServerReference{{ID: "search", Enabled: true}},
		Hooks:      sessionfs.HooksSettings{PreTool: []sessionfs.ToolEventHook{{Name: "toolset-hook", Match: "*", Argv: []string{"true"}}}},
	}
	other := sessionfs.SessionConfig{WorkingDir: "/other"}
	agents := []sessionfs.NativeSubagentAgent{
		{Name: "a", Toolset: []string{"extra/search"}},
		{Name: "b", Toolset: []string{"extra/search"}, Sandbox: execBox},
		{Name: "boxed", Toolset: []string{"extra/search"}, Sandbox: agentBox},
		{Name: "o", Toolset: []string{"other"}},
		{Name: "plain"},
	}
	owner := sessionfs.SessionConfig{
		Model:          sessionfs.Model{Name: "owner", APIKeyEnv: "OWNER_KEY"},
		NativeAsync:    &sessionfs.NativeAsyncSettings{},
		AgentToolsets:  map[string]sessionfs.SessionConfig{"extra/search": loaded, "other": other},
		NativeSubagent: &sessionfs.NativeSubagentSettings{Agents: agents},
	}
	before := reflect.ValueOf(owner.AgentToolsets["extra/search"]).Interface()

	configs := NewAgentToolConfigs(owner)
	want := loaded
	want.Model = owner.Model
	if !reflect.DeepEqual(configs.Native["extra/search"], want) || !reflect.DeepEqual(configs.Native["other"].Model, owner.Model) || len(configs.Native) != 2 {
		t.Fatalf("native configs = %+v", configs.Native)
	}
	if !reflect.DeepEqual(owner.AgentToolsets["extra/search"], before) || owner.AgentToolsets["extra/search"].Model.Name != "" {
		t.Fatal("the owner's loaded toolsets changed")
	}

	boxedKey := ToolsetMCPKey(agents[2])
	if len(configs.MCP) != 3 || configs.MCP[0].Key != "extra/search" || configs.MCP[1].Key != boxedKey || configs.MCP[2].Key != "other" {
		t.Fatalf("MCP views = %+v", configs.MCP)
	}
	shared, _ := configs.MCPView("extra/search")
	boxed, _ := configs.MCPView(boxedKey)
	if !reflect.DeepEqual(shared.Config, want) || shared.Toolset != "extra/search" {
		t.Fatalf("shared view = %+v", shared)
	}
	boxedWant := want
	boxedWant.Sandbox = agentBox
	if !reflect.DeepEqual(boxed.Config, boxedWant) || boxed.Toolset != "extra/search" {
		t.Fatalf("boxed view = %+v", boxed)
	}
	if _, found := configs.MCPView("missing"); found {
		t.Fatal("a missing key has a view")
	}

	// Without the owner's async section the toolset has no async tool, for the MCP view too.
	owner.NativeAsync = nil
	if configs := NewAgentToolConfigs(owner); configs.Native["extra/search"].NativeAsync != nil || configs.MCP[0].Config.NativeAsync != nil {
		t.Fatal("async kept without the owner's async section")
	}
	// Without a subagent section there are native configs but no servers to start for agents.
	owner.NativeSubagent = nil
	if configs := NewAgentToolConfigs(owner); configs.MCP != nil || len(configs.Native) != 2 {
		t.Fatalf("without agents: %+v", configs)
	}
	if configs := NewAgentToolConfigs(sessionfs.SessionConfig{NativeSubagent: &sessionfs.NativeSubagentSettings{Agents: agents}}); configs.Native != nil || configs.MCP != nil {
		t.Fatal("no toolsets, yet configs")
	}
}
