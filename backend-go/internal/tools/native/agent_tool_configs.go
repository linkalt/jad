package native

import (
	"maps"
	"slices"

	"jad/backend-go/internal/sessionfs"
)

// AgentToolConfigs is the tool side of an owner run's agents: the configurations their
// toolsets' tools are built from, as one owner run's prepare sees them. It is derived from the
// owner run's configuration alone, the same one the run's sub-agent launcher starts every child
// (and every nested child, which reuses the owner's subagent tool) over, so the native toolset
// tools, the toolset MCP servers and a run's own toolset MCP servers all come from this one
// derivation. The toolsets themselves are composed and checked at load
// (sessionfs.SessionConfig.AgentToolsets); a child's own run configuration is finished when it
// starts, because the call may name its model (withCallModel) and loading the model file at
// prepare would move its errors.
type AgentToolConfigs struct {
	// Native is, by sessionfs.NativeSubagentAgent.ToolsetKey, the configuration a toolset's
	// native tools are built from.
	Native map[string]sessionfs.SessionConfig
	// MCP is, sorted by ToolsetMCPKey, each set of toolset MCP servers the agents use.
	MCP []AgentMCPView
}

// AgentMCPView is one set of toolset MCP servers: its ToolsetMCPKey, the toolset key, and the
// toolset's configuration with the sandbox its stdio servers start in.
type AgentMCPView struct {
	Key, Toolset string
	Config       sessionfs.SessionConfig
}

// NewAgentToolConfigs derives the tool side of owner's agents.
//
// Native: the toolset was loaded before the model file was read, so it has no model; it gets
// the owner run's, so its bash, exec and process hide the running model's key from a child's
// environment the way the session's do. A toolset shares the owner's async host, so without
// tools.native.async in the owner it has no async tool (the host exists exactly when the
// owner has that section; its opening failing fails the whole catalog).
//
// MCP: one set per toolset for the agents without a sandbox of their own that covers mcp, and
// one per agent with one, in that agent's sandbox (U4).
func NewAgentToolConfigs(owner sessionfs.SessionConfig) AgentToolConfigs {
	var configs AgentToolConfigs
	if len(owner.AgentToolsets) == 0 {
		return configs
	}
	configs.Native = make(map[string]sessionfs.SessionConfig, len(owner.AgentToolsets))
	for key, derived := range owner.AgentToolsets {
		derived.Model = owner.Model
		if owner.NativeAsync == nil {
			derived.NativeAsync = nil
		}
		configs.Native[key] = derived
	}
	if owner.NativeSubagent == nil {
		return configs
	}
	byKey := make(map[string]AgentMCPView)
	for _, agent := range owner.NativeSubagent.Agents {
		key := ToolsetMCPKey(agent)
		derived, exists := configs.Native[agent.ToolsetKey()]
		if key == "" || !exists {
			continue
		}
		if agent.Sandbox.Covers("mcp") {
			derived.Sandbox = agent.Sandbox
		}
		byKey[key] = AgentMCPView{Key: key, Toolset: agent.ToolsetKey(), Config: derived}
	}
	if len(byKey) == 0 {
		return configs
	}
	configs.MCP = make([]AgentMCPView, 0, len(byKey))
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		configs.MCP = append(configs.MCP, byKey[key])
	}
	return configs
}

// MCPView is the set of toolset MCP servers with key.
func (c AgentToolConfigs) MCPView(key string) (AgentMCPView, bool) {
	for _, view := range c.MCP {
		if view.Key == key {
			return view, true
		}
	}
	return AgentMCPView{}, false
}
