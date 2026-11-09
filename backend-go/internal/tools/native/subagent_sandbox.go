package native

import (
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// An agent with a sandbox of its own (agent key sandbox) runs its tools in that sandbox
// (childEnvironment), so what its model reads about the sandbox, and where its toolset's MCP
// servers start, follow the agent's sandbox, not the session's.

// sandboxDescribedTools are the tools whose description ends with the sandbox sentence
// (sandboxDescription), by tool ID, with the name the sandbox's tools list covers them by.
var sandboxDescribedTools = map[string]string{execToolID: "exec", toolSmithToolID: "tool_smith"}

// sandboxBaseDescriptions are the descriptions of the sandbox-described tools as their
// definition files give them, before the sandbox sentence is added, by tool ID.
func sandboxBaseDescriptions(definitions map[string]tools.Definition) map[string]string {
	bases := make(map[string]string, len(sandboxDescribedTools))
	for id := range sandboxDescribedTools {
		if definition, exists := definitions[id]; exists {
			bases[id] = definition.Description
		}
	}
	return bases
}

// describeAgentSandbox is pool with the description of each sandbox-described tool told the
// agent's sandbox instead of the one the tool was built with: the base text plus the sentence
// for sandbox when it covers the tool, and no sentence when it does not, as callSandbox then
// starts the call outside any sandbox. pool itself is not changed. A nil sandbox, or a tool
// whose base text is unknown, keeps the tool as it is.
func describeAgentSandbox(pool []tools.Tool, sandbox *sessionfs.SandboxSettings, bases map[string]string) []tools.Tool {
	if sandbox == nil {
		return pool
	}
	described := append([]tools.Tool(nil), pool...)
	for index, tool := range described {
		name, covered := sandboxDescribedTools[tool.Definition.ID]
		base, known := bases[tool.Definition.ID]
		if !covered || !known {
			continue
		}
		var applies *sessionfs.SandboxSettings
		if sandbox.Covers(name) {
			applies = sandbox
		}
		described[index].Definition.Description = base + sandboxDescription(applies)
	}
	return described
}

// ToolsetMCPKey is the key of the MCP servers an agent that names a toolset uses: the
// toolset's key, shared by every agent of that toolset, or, when the agent's own sandbox
// covers mcp, the toolset's key and the agent's name, so the servers start in that agent's
// sandbox. Keying by the agent and not by the sandbox keeps one set of servers per agent: an
// edited sandbox restarts them (the manager compares the wrapper key) instead of leaving the
// old ones running. Empty for an agent without a toolset (it uses the session's servers, D10).
func ToolsetMCPKey(agent sessionfs.NativeSubagentAgent) string {
	key := agent.ToolsetKey()
	if key == "" || !agent.Sandbox.Covers("mcp") {
		return key
	}
	return key + "\x00agent\x00" + agent.Name
}
