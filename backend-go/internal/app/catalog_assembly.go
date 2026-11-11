package app

import (
	"fmt"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// catalogParts are what a profile's registered catalog is assembled from. buildRegisteredCatalog
// acquires them: the shared task and records stores and the subagent manager the native catalog
// holds, the MCP manager it configures and whose servers it starts, the toolsets' managers and
// servers. Assembly only joins them.
type catalogParts struct {
	// sections are the owner's native sections (native.CatalogSections: native and extension
	// tools, subagent, profile_message, load_tool), built with subagentMCP and lazy below; assembly
	// adds the MCP section and composes them.
	sections native.CatalogSections
	// subagentMCP is the subagent tool's binding, made for this build by NewAssembledSubagentMCP
	// (nil-safe when the profile has no subagent tool): its agents are resolved, their catalogs are
	// not built until assembly gives it the lazy set, the run MCP loader and the MCP copies.
	subagentMCP *native.SubagentMCP
	// lazy is the profile's lazy set for this build, nil without tools.lazy; the native pass shares it.
	lazy *tools.LazySet
	// mcpTools are the profile's MCP tools as their manager listed them: neither gated nor hooked.
	mcpTools []tools.Tool
	// toolsetMCP are the raw MCP tools of each agent toolset, with that toolset's hooks.
	toolsetMCP map[string]native.ToolsetMCP
	// runMCP starts the stdio servers of a sub-agent run in a worktree of its own (run_mcp.go).
	runMCP native.RunMCPLoader
}

// assembleRegisteredCatalog joins the parts into the profile's registered catalog: one copy of
// each MCP tool, gated with the lazy set and hooked by its hook owner (native.MCPCopies), held by
// the profile's catalog and bound into the subagent tool for its children; the children's catalogs
// built from the same parts; the agent list redescribed, the owner's sections composed once and
// the hooks' matches checked (warn reports a hook naming a disabled server).
//
// The owner's catalog and each child's are composed the same way (native.CatalogSections, one
// order) from shared parts: the owner's native tools are each child's pool, its subagent tool is
// each nesting child's (hooked again with the owner's hooks), its load_tool is the very tool a
// child that shows load_tool holds, and its MCP copies are the children's. Each side keeps its own
// policy: a child's sections are filtered by its agent when they are built, the owner's catalog is
// narrowed per prepare (buildToolCatalog).
//
// It takes no resolver: it starts no server, sets nothing on an MCP manager (wrapper, route key,
// sampler), opens no store and registers no resource. What it changes are parts.lazy (the MCP
// tools are bound into it) and parts.subagentMCP, which belong to this one build; the listed
// tools themselves are not changed. So a build that acquired its parts in another way (another
// manager, a narrowing's servers) assembles them the same way.
func assembleRegisteredCatalog(config sessionfs.SessionConfig, parts catalogParts, warn func(string)) (*tools.Catalog, error) {
	// Every MCP tool is gated (its lazy tools only) and hooked once for the whole build: a session
	// tool with the profile's hooks, a toolset's with that toolset's hooks. The profile's catalog
	// holds the session copies, and each child catalog holds the same copies its agent shows
	// (AssembleChildren), so the owner and its children share one copy of each tool.
	parts.lazy.Bind(parts.mcpTools)
	copies := native.NewMCPCopies(parts.mcpTools, parts.toolsetMCP, parts.lazy, config.Hooks, config.Model.APIKeyEnv)
	owner := parts.sections
	owner.MCP = copies.Session()
	// The children's catalogs are assembled here, from the same parts as the owner's: the lazy set,
	// the owner's own load_tool (nil when the owner enables none; built from the owner's set and
	// hooks, so a child lists, describes and runs the owner's lazy tools), the copies the owner's
	// catalog holds, and the loader that starts an isolated run's stdio MCP servers in its worktree
	// (run_mcp.go). Each agent's catalogs are built once (the subagent tool was made by
	// NewAssembledSubagentMCP, so it built none), each with the agent's own filter; the owner's
	// allow list and strict entries apply to its own catalog only (buildToolCatalog).
	subagentMCP := parts.subagentMCP
	if err := subagentMCP.AssembleChildren(native.ChildParts{Lazy: parts.lazy, LoadTool: owner.LoadTool, RunMCP: parts.runMCP, MCP: copies}); err != nil {
		return nil, mapSessionError(&sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)})
	}
	// The agent list names which agents change files, which depends on the MCP tools just bound.
	registered, err := subagentMCP.RedescribeSections(owner).Compose()
	if err != nil {
		return nil, mapSessionError(err)
	}
	if err := checkHookMatches(config, registered.Definitions(), warn); err != nil {
		return nil, mapSessionError(err)
	}
	return registered, nil
}
