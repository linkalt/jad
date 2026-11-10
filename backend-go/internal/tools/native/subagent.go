package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const subagentToolID = "native.subagent"

// subagentModelProperty is the schema line of the model argument; the tool has it only with
// tools.native.subagent.model_choice on.
const subagentModelProperty = `
    "model":{"type":"string","description":"Model ID for this run, optionally with @<effort>: one of the models listed below. Omit to use the agent's model, or the session's model default when the agent names none."},`

// subagentSandboxProperty is the schema line of the sandbox argument; the tool has it only with
// tools.native.subagent.sandbox_choice set.
const subagentSandboxProperty = `
    "sandbox":{"type":"string","description":"Sandbox template for this run: one of the sandboxes listed below. Omit to use the agent's own sandbox, or the session's when the agent names none. A template can only narrow what the run may read, write and reach; one that would widen it is refused."},`

var subagentInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "task":{"type":"string","minLength":1,"description":"Self-contained instructions. The agent sees nothing of this conversation. Omit to list the agents."},
    "agent":{"type":"string","description":"Agent name. Optional when only one is configured."},` + subagentModelProperty + subagentSandboxProperty + `
    "resume":{"type":"string","description":"Run id (sub_...) of a finished run of this session to continue from its transcript. The new run is a copy: the old run is not changed, and the result names it as resumed_from."},
    "block_ms":{"type":"integer","minimum":0,"description":"Most time this call blocks for the answer; 0 returns a run id at once. Clamped to the async policy. An agent marked [can change files; runs to its end] ignores it and runs to its end."}
  },
  "additionalProperties":false
}`)

const subagentDescription = "Hand a task to a separate agent with its own fresh context and get back only its final answer. Agents only read unless marked [can change files]. Use it for searches or reading that would fill your context. A slow agent returns a run id while it keeps working; never start the task again: use the async tool with that id to wait for it, look at it or stop it. resume continues a finished run of this session as a new run. [can change files; runs to its end] is never handed off. [can change files; may run in the background] is handed off, keeps editing after your turn ends and cannot ask the user: leave its paths alone until it ends."

// subagentExample is shown to the model when its arguments are rejected.
var subagentExample = json.RawMessage(`{"task":"Find where the retry delay is computed and report the file paths and line numbers."}`)

// SubagentRequest is one child run: the agent to start, its task, and the tools it may call.
type SubagentRequest struct {
	Agent sessionfs.NativeSubagentAgent
	Task  string
	// CallID is the parent's tool call, used to name the child's transcript.
	CallID string
	// Tools is the child's catalog: every tool of the session, narrowed only by the profile's tools.deny and the agent's own tools lists (the nested subagent tool is absent at the deepest level).
	Tools *tools.Catalog
	// Execution carries the workspace roots the child's tools resolve paths against.
	Execution tools.ExecutionContext
	// OnTranscript, when set, is called once with the child's transcript directory as
	// soon as the launcher has created it, so the manager knows where the child's work
	// is even if the server dies before the child ends.
	OnTranscript func(dir string)
	// Seed, when set, is the transcript of the run this one resumes: the launcher copies it
	// into the new run's own folder before it opens the journal, and never writes to it.
	Seed string
	// CallerValues is the calling tool call's context, kept only for its values (the
	// calling run's cost meter, so the child's spend counts toward the caller's
	// max_run_cost_usd and budget). The child's cancellation does not come from it: a run
	// hangs off the manager. Nil when there is no caller.
	CallerValues context.Context
}

// SubagentResult is what a finished child run reports.
type SubagentResult struct {
	// Text is the child's final answer.
	Text string
	// Rounds is the number of provider responses the child used.
	Rounds int
	// Tokens is the usage the provider reported for the child, summed over its rounds.
	Tokens int64
}

// SubagentLauncher runs one child to completion. The app package implements it,
// because starting a run needs the model, credential and transcript wiring that the
// tool package does not know. ctx ends when the parent run stops or the agent's
// timeout passes; the launcher must stop promptly then.
type SubagentLauncher interface {
	Run(ctx context.Context, request SubagentRequest) (SubagentResult, error)
}

func (a subagentAgent) hasNativeTool(id string) bool {
	for _, tool := range a.nativeTools {
		if tool.Definition.ID == id {
			return true
		}
	}
	return false
}

type subagentAgent struct {
	config sessionfs.NativeSubagentAgent
	// catalog is what the child may call; leaf is the same without the subagent tool, for a
	// child that is at the deepest level (see buildCatalogs). Both are nil for an agent whose allow
	// list names MCP tools until SubagentMCP.Bind has seen the loaded MCP tools.
	catalog *tools.Catalog
	leaf    *tools.Catalog
	// filter is the agent's own narrowing, written as a profile's tools block (see
	// NativeSubagentAgent.ToolSettings): what a profile's applyProfileTools would apply to it.
	filter sessionfs.ProfileToolSettings
	// nativeTools are the pool's native tools the filter shows, without subagent and load_tool
	// (buildCatalogs adds them); mcpTools are the MCP tools it shows once bound (bindMCP).
	nativeTools []tools.Tool
	mcpTools    []tools.Tool
	// needsMCP is true when the allow list names MCP tools, so the catalog waits for bindMCP;
	// mcpBound is true once it ran.
	needsMCP bool
	mcpBound bool
	// nests is true when a child of this agent may be offered the subagent tool (max_depth is 2 or
	// more and the agent's tools show it). Its calls can then reach any agent, local or remote, so
	// the tool classifies it by what it may start (nestsWriter), not by its own tools alone.
	nests bool
	// background is set when a call to this agent may run as an async handle although
	// the agent can change files; see backgroundSafe.
	background bool
	// descriptions are the base descriptions of the sandbox-described tools of its pool
	// (sandboxBaseDescriptions), for the catalog of a sandbox choice (choiceAgent).
	descriptions map[string]string
}

// changesWorkspace reports whether a run of the agent can change the session's own files
// (NativeSubagentAgent.ChangesWorkspace). A worktree of its own or a sandbox that writes only
// scratch does not make an agent with MCP tools a reader, although an isolated run starts its
// stdio MCP servers in its worktree (run_mcp.go, D10 and P6): a streamable HTTP server is a
// remote endpoint the run cannot move, and a stdio server whose session has no sandbox that lists
// mcp is only started in another folder, not kept out of the session's. Such a run is scheduled
// and checkpointed like any writer. An agent that has an MCP tool not marked ReadOnly is a writer
// whatever its native tools are (mcpWrites).
func (a subagentAgent) changesWorkspace() bool {
	if a.config.ChangesWorkspace() || a.mcpWrites() {
		return true
	}
	return a.config.ChangesFiles() && len(a.mcpTools) > 0
}

// changesFiles reports whether the agent can change files: it has a native tool that can
// (NativeSubagentAgent.ChangesFiles) or an MCP tool that may (mcpWrites).
func (a subagentAgent) changesFiles() bool {
	return a.config.ChangesFiles() || a.mcpWrites()
}

// mcpWrites reports whether the agent shows an MCP tool that is not marked ReadOnly. A server's
// own claim (readOnlyHint) is not trusted and an unknown tool is not assumed to read, so only the
// resources and prompts tools, which the protocol makes read-only, leave an agent a reader. The
// tools are the agent's own copies, so an agent whose list names only MCP tools and one with no
// list at all are judged the same way, and so is a tool it loads through load_tool (a lazy tool is
// still one of its mcpTools).
func (a subagentAgent) mcpWrites() bool {
	for _, tool := range a.mcpTools {
		if !tool.ReadOnly {
			return true
		}
	}
	return false
}

// backgroundSafe reports whether a writer agent may run as a handle: the session turned
// background_writers on, the async tool is there to collect the run, and the agent
// has a tool that changes the session's files. An agent with isolation worktree is
// handed off like a reader without it.
func backgroundSafe(settings sessionfs.NativeSubagentSettings, agent subagentAgent, host *AsyncHost) bool {
	if !settings.BackgroundWriters || host == nil || !agent.changesWorkspace() {
		return false
	}
	return true
}

// mcpToolIDPrefix starts the name an agent's tools list uses for an MCP tool:
// mcp:<server>:<tool>, the tool's stable ID. The name the model sees ends in a hash
// and is not meant to be written by hand.
const mcpToolIDPrefix = "mcp:"

// SubagentMCP hands the loaded MCP tools to the subagent tool. The native catalog is
// built before the MCP servers are listed, and the subagent tool is part of it, so
// the MCP tools reach it afterwards, once, through Bind.
type SubagentMCP struct {
	tool *subagentTool
	// hooks are the owning profile's pre_tool and post_tool hooks. The subagent tool hands
	// its nested copy to each child with them, so a nested call fires them as the parent's does.
	hooks     sessionfs.HooksSettings
	apiKeyEnv string
}

// NewSubagentMCP is the handle for a session whose subagent tool is built with the owning
// profile's hooks. Every nested copy is hooked from the start, so no child catalog ever holds the raw one.
func NewSubagentMCP(hooks sessionfs.HooksSettings, apiKeyEnv string) *SubagentMCP {
	return &SubagentMCP{hooks: hooks, apiKeyEnv: apiKeyEnv}
}

// Bind gives each agent that lists MCP tools its catalog. It fails when a listed tool
// is not among the loaded ones. It does nothing when the session has no subagent tool.
func (m *SubagentMCP) Bind(mcpTools []tools.Tool) error {
	return m.BindToolsets(mcpTools, nil)
}

// SetLazy gives the owning profile's lazy set to the children: each child's MCP tools in that set are
// gated with it. loadTool is the child's load_tool, built by the owner from the
// same set and manifest; nil when the owner enables no load_tool. A child gets it when its own
// settings show it, as a profile gets its own.
func (m *SubagentMCP) SetLazy(lazy *tools.LazySet, loadTool *tools.Tool) error {
	if m == nil || m.tool == nil {
		return nil
	}
	m.tool.lazy = lazy
	m.tool.loadTool = loadTool
	if loadTool != nil {
		return m.tool.buildAllCatalogs()
	}
	return nil
}

// SetRunMCP gives the subagent tool the loader that starts the stdio MCP servers of an isolated
// run: a run in a worktree of its own (and a child that works inside it) calls its MCP tools on
// servers started in that worktree, and never on the session's. Without it every run uses the
// session's servers (docs/isolation-decisions.md D10).
func (m *SubagentMCP) SetRunMCP(load RunMCPLoader) {
	if m == nil || m.tool == nil {
		return
	}
	m.tool.mu.Lock()
	defer m.tool.mu.Unlock()
	m.tool.runMCP = load
}

// ToolsetMCP is the MCP tools loaded for one toolset: the raw tools, as the toolset's own manager
// listed them, and the hooks of the toolset's profile. The subagent tool gates a tool with the
// owner's lazy set and then hooks it with Hooks, so the hooks wrap the gate as they do for the owner.
type ToolsetMCP struct {
	Tools []tools.Tool
	Hooks sessionfs.HooksSettings
}

// BindToolsets is Bind plus the MCP tools loaded for each toolset an agent names, by
// ToolsetMCPKey (the toolset ID, with the agent's name when they start in its own sandbox). An
// agent with a toolset resolves its mcp: entries against those tools only, never the session's.
func (m *SubagentMCP) BindToolsets(mcpTools []tools.Tool, toolsetTools map[string]ToolsetMCP) error {
	if m == nil || m.tool == nil {
		return nil
	}
	return m.tool.bindMCP(mcpTools, toolsetTools, m.hooks, m.hookWith)
}

// hookTool wraps a nested copy of a tool with the owning profile's hooks. A nil handle returns the
// tool unchanged, as newSubagentToolRemotes builds a tool without one.
func (m *SubagentMCP) hookTool(tool tools.Tool) tools.Tool {
	if m == nil {
		return tool
	}
	return m.hookWith(m.hooks, tool)
}

// hookWith wraps a nested copy of a tool with the given profile's hooks (a toolset's, for its tool).
func (m *SubagentMCP) hookWith(hooks sessionfs.HooksSettings, tool tools.Tool) tools.Tool {
	if m == nil {
		return tool
	}
	return WithToolEventHooks(tool, hooks, m.apiKeyEnv)
}

type subagentTool struct {
	// self is the tool as the owner built it, before any hook; nestedSelf makes each nesting child's
	// copy from it.
	self tools.Tool
	// mcp hooks the nested copies with the owning profile's hooks; nil when the caller has no handle.
	mcp      *SubagentMCP
	settings sessionfs.NativeSubagentSettings
	// mu guards the agents' catalogs, which Bind fills in after the tool is built.
	mu       sync.RWMutex
	agents   []subagentAgent
	launcher SubagentLauncher
	// manager owns the runs, so a run outlives the call that started it.
	manager *SubagentManager
	// async makes read-only runs handles of the async manager; nil when the session has no
	// async tool.
	async *AsyncHost
	// remotes is the other JAD servers a call may start an agent on; empty offers none.
	remotes remoteAgentSettings
	// lazy is the owning profile's lazy set and loadTool its load_tool, both given by SubagentMCP.SetLazy;
	// nil when the owner has no tools.lazy or no load_tool.
	lazy     *tools.LazySet
	loadTool *tools.Tool
	// runMCP starts the stdio MCP servers of an isolated run's own (SubagentMCP.SetRunMCP); nil
	// leaves every run on the session's servers. Guarded by mu.
	runMCP RunMCPLoader
	// choiceCatalogs are the agents' catalogs for the sandbox choices calls made, by agent and
	// choice name (choiceAgent); choiceMu guards them.
	choiceMu       sync.Mutex
	choiceCatalogs map[string]choiceCatalogs
}

// NewSubagentTool builds the subagent tool over the already built tools of the same
// catalog. A child is just another profile: it has every one of them (and the subagent tool, bounded
// by max_depth), narrowed only by the session profile's tools.deny and its agent's own tools
// allow and deny lists.
func NewSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher) (tools.Tool, error) {
	return NewSubagentToolWithMCP(settings, parentTools, launcher, nil)
}

// NewSubagentToolWithMCP is NewSubagentTool plus the hook that later gives the tool
// the MCP tools a child may use. A nil mcp is allowed; an agent whose allow list names an MCP tool
// then cannot start a child.
func NewSubagentToolWithMCP(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher, mcp *SubagentMCP) (tools.Tool, error) {
	return NewSubagentToolWithManager(settings, parentTools, launcher, mcp, nil)
}

// NewSubagentToolWithManager is NewSubagentToolWithMCP plus the manager that owns the
// runs. The resolver passes one shared by every session of a storage directory and
// closes it at shutdown. A nil manager gives the tool a private one that nothing
// closes, which suits tests only.
func NewSubagentToolWithManager(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager) (tools.Tool, error) {
	return newSubagentTool(settings, parentTools, nil, launcher, mcp, manager)
}

func newSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager) (tools.Tool, error) {
	return newSubagentToolAsync(settings, parentTools, toolsets, launcher, mcp, manager, nil)
}

// subagentToolset is the native tools built from one toolset file an agent names. The
// parent's model never sees them.
type subagentToolset struct {
	native []tools.Tool
	// descriptions are the base descriptions of its sandbox-described tools (sandboxBaseDescriptions).
	descriptions map[string]string
}

// childNativePool is the native and extension tools a child may be offered: every one the owner
// built, except subagent and load_tool, which the owner hands to each child itself (nestedSelf,
// SetLazy) so a child's copy is the owner's. MCP tools come through bindMCP.
func childNativePool(available []tools.Tool) []tools.Tool {
	pool := make([]tools.Tool, 0, len(available))
	for _, tool := range available {
		if tool.Definition.Source != "native" && tool.Definition.Source != "extension" {
			continue
		}
		if tool.Definition.ID == subagentToolID || tool.Definition.ID == loadToolID {
			continue
		}
		pool = append(pool, tool)
	}
	return pool
}

// newSubagentToolAsync is newSubagentTool plus the async host that tracks every read-only run
// the tool starts as a handle owned by the calling session. A nil host starts no handles.
func newSubagentToolAsync(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, host *AsyncHost) (tools.Tool, error) {
	return newSubagentToolRemotes(settings, parentTools, toolsets, launcher, mcp, manager, host, nil)
}

// newSubagentToolRemotes is newSubagentToolAsync plus the remotes folder the session's
// tools.native.subagent.remotes names are read from.
func newSubagentToolRemotes(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, host *AsyncHost, remotes *remote.Directory) (tools.Tool, error) {
	return newSubagentToolDescribed(settings, parentTools, nil, toolsets, launcher, mcp, manager, host, remotes)
}

// newSubagentToolDescribed is newSubagentToolRemotes plus the base descriptions of the
// session's sandbox-described tools, so an agent with a sandbox of its own reads that sandbox
// in its exec and tool_smith descriptions (describeAgentSandbox). Without them the agent sees
// the session's descriptions.
func newSubagentToolDescribed(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, descriptions map[string]string, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, host *AsyncHost, remotes *remote.Directory) (tools.Tool, error) {
	schema := optionalPropertiesSchema(subagentInputSchema, []optionalProperty{{subagentModelProperty, settings.ModelChoice}, {subagentSandboxProperty, settings.SandboxChoice != nil}})
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native subagent schema: %w", err)
	}
	if settings.MaxConcurrent < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.max_concurrent must be at least 1")
	}
	if settings.KeepFinished < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.keep_finished must be at least 1")
	}
	if manager == nil {
		manager = NewSubagentManager()
	}
	pool := childNativePool(parentTools)
	runtime := &subagentTool{settings: settings, launcher: launcher, manager: manager, async: host, mcp: mcp}
	if remotes != nil && len(settings.Remotes) > 0 {
		runtime.remotes = remoteAgentSettings{names: settings.Remotes, directory: remotes, timeout: time.Duration(settings.RemoteTimeoutMs) * time.Millisecond, snapshot: takeRemoteSnapshot(remotes, settings.Remotes)}
	}
	if host != nil {
		host.attachSubagent(manager)
		if len(runtime.remotes.names) > 0 {
			host.attachRemoteSubagent()
		}
	}
	for _, agent := range settings.Agents {
		agentPool, agentDescriptions := pool, descriptions
		toolsetKey := agent.ToolsetKey()
		if toolsetKey != "" {
			toolset, built := toolsets[toolsetKey]
			if !built {
				return tools.Tool{}, fmt.Errorf("tools.native.subagent.agents.%s.toolset %q was not loaded", agent.Name, toolsetKey)
			}
			agentPool, agentDescriptions = childNativePool(toolset.native), toolset.descriptions
			// A toolset cannot set profile_message; the session's tool is offered to the agent as it is to the others.
			for _, tool := range pool {
				if tool.Definition.ID == profileMessageToolID {
					agentPool = append(agentPool[:len(agentPool):len(agentPool)], tool)
				}
			}
		}
		agentPool = describeAgentSandbox(agentPool, agent.Sandbox, agentDescriptions)
		if agent.Isolated() {
			agentPool = describeAgentIsolation(agentPool)
		}
		entry, err := newSubagentAgent(agent, settings.DenyTools, agentPool, settings.MaxDepth >= 2)
		if err != nil {
			if toolsetKey != "" {
				err = fmt.Errorf("%w (its tools come from toolset %s)", err, toolsetKey)
			}
			if len(settings.DenyTools) > 0 {
				err = fmt.Errorf("%w (the profile's tools.deny is in effect: %s)", err, strings.Join(settings.DenyTools, ", "))
			}
			return tools.Tool{}, err
		}
		entry.background = backgroundSafe(settings, entry, host)
		entry.descriptions = agentDescriptions
		runtime.agents = append(runtime.agents, entry)
	}
	if mcp != nil {
		mcp.tool = runtime
	}
	self := tools.Tool{
		Definition: tools.Definition{
			ID:          subagentToolID,
			Source:      "native",
			Name:        "subagent",
			InputSchema: schema,
		},
		Validator:        validator,
		ExampleOperation: subagentExample,
		// A read-only child cannot change files and the tool keeps its own state outside
		// the workspace, so several calls of one response may run side by side. A call
		// to an agent that may change files says so through MayChangeFiles, and then
		// takes the checkpoint and runs in order like a write.
		ReadOnly:           true,
		MayChangeFiles:     runtime.callMayChangeFiles,
		PlanModeRefusal:    runtime.planModeRefusal,
		NormalizeArguments: normalizeSubagentArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return runtime.execute(ctx, arguments, execution)
		},
	}
	runtime.self = self
	// The description marks what each agent may change, which needs whether it nests; the MCP tools an
	// agent may have are not bound yet, and bindMCP describes the tool again once they are.
	for index := range runtime.agents {
		runtime.agents[index].nests = runtime.nestsFor(runtime.agents[index])
	}
	self.Definition.Description = runtime.description()
	runtime.self = self
	if err := runtime.buildAllCatalogs(); err != nil {
		return tools.Tool{}, err
	}
	return self, nil
}

// description is the tool's model-facing description: what it does, the agents with what each may
// change (agentMarker), the remotes and the model and sandbox choices. The caller holds s.mu, or no
// other goroutine has the tool yet.
func (s *subagentTool) description() string {
	lines := make([]agentLine, len(s.agents))
	for index, agent := range s.agents {
		lines[index] = agentLine{name: agent.config.Name, marker: s.agentMarker(agent), description: agent.config.Description}
	}
	list := subagentListText(s.settings, lines)
	return subagentDescription + list + remoteListText(s.remotes.names, s.sameHostRemotes()) + subagentModelText(s.settings) + sandboxChoiceText(s.settings.SandboxChoices)
}

// Redescribe is catalog with the subagent tool's description as it is now. The tool is built with the
// native catalog, before the MCP tools exist, and whether an agent changes files depends on them
// (mcpWrites), so the owner calls this once after Bind. A catalog without the tool is returned as it is.
func (m *SubagentMCP) Redescribe(catalog *tools.Catalog) (*tools.Catalog, error) {
	if m == nil || m.tool == nil {
		return catalog, nil
	}
	m.tool.mu.RLock()
	text := m.tool.description()
	m.tool.mu.RUnlock()
	return catalog.WithDescription(subagentToolID, text)
}

// nestsFor reports whether a child of agent may be offered the subagent tool: max_depth is 2 or more
// and the agent's tools show it.
func (s *subagentTool) nestsFor(agent subagentAgent) bool {
	return s.settings.MaxDepth >= 2 && tools.ProfileShows(agent.filter, s.self.Definition)
}

// agentMarker is the marker the agent list shows for an agent, from the way the tool classifies its
// calls (callWrites): an agent that changes the session's files through a native tool or an MCP tool
// that is not read-only, or that may start one that does (nestsWriterLocked), is marked as a writer
// whatever its own config says, and runs to its end unless it may be handed off. Any other agent is
// marked as its config says (changeMarker). The caller holds s.mu.
func (s *subagentTool) agentMarker(agent subagentAgent) string {
	if agent.changesWorkspace() || s.nestsWriterLocked(agent) {
		if agent.background {
			return " [can change files; may run in the background]"
		}
		return " [can change files; runs to its end]"
	}
	return changeMarker(agent.config, agent.background)
}

// nestedSelf is the subagent tool as a nesting child gets it: the owner's tool, hooked with the owner's
// hooks. A native tool is never lazy, so it is not gated.
func (s *subagentTool) nestedSelf() tools.Tool {
	return s.mcp.hookTool(s.self)
}

// buildCatalogs builds the two catalogs of an agent from its native tools, the owner's load_tool
// (when the agent's settings show it) and the MCP tools bound so far. catalog is what a child gets
// that may still start a child of its own: it holds the subagent tool when the agent's settings show
// it and max_depth is 2 or more. leaf is the same without it, for a child at the deepest level,
// where a call would only be refused (startRunFrom picks one by depth; the depth check stays the
// gate). An agent that lists MCP tools has no catalog until bindMCP has seen them.
func (s *subagentTool) buildCatalogs(agent *subagentAgent) error {
	agent.nests = s.nestsFor(*agent)
	if agent.needsMCP && !agent.mcpBound {
		agent.catalog, agent.leaf = nil, nil
		return nil
	}
	leaf := append([]tools.Tool(nil), agent.nativeTools...)
	var loadTool []tools.Tool
	if s.loadTool != nil && tools.ProfileShows(agent.filter, s.loadTool.Definition) {
		loadTool = []tools.Tool{*s.loadTool}
	}
	leaf = append(append(leaf, loadTool...), agent.mcpTools...)
	full := leaf
	if agent.nests {
		full = append(append(append(append([]tools.Tool(nil), agent.nativeTools...), s.nestedSelf()), loadTool...), agent.mcpTools...)
	}
	var err error
	if agent.leaf, err = tools.NewCatalog(leaf); err != nil {
		return fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
	}
	agent.catalog = agent.leaf
	if len(full) != len(leaf) {
		if agent.catalog, err = tools.NewCatalog(full); err != nil {
			return fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
		}
	}
	return nil
}

// buildAllCatalogs builds the catalog of every agent; the caller holds no lock.
func (s *subagentTool) buildAllCatalogs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.agents {
		if err := s.buildCatalogs(&s.agents[index]); err != nil {
			return err
		}
	}
	return nil
}

func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// callMayChangeFiles is the tool's per-call answer to "can this call change files", the one the
// checkpoint and ordered scheduling read (Catalog.ChangesState): see callWrites, here with what the
// agent may start below it counted.
func (s *subagentTool) callMayChangeFiles(arguments map[string]any) bool {
	return s.callWrites(arguments, true)
}

// callWrites says whether the call can change the files of the session's host. A call can when the
// agent it picks lists a tool that changes files and works in the session's own folder (an agent with
// isolation worktree changes only its worktree), or has an MCP tool that is not marked ReadOnly
// (changesWorkspace). A call that picks no agent, or only checks a run, cannot. A resume that names no
// agent picks the agent of the run it continues.
//
// below says whether to count what the agent may start: no run of a child has a checkpoint of its own
// and a child's write is covered only by the checkpoint of the call at the top, so an agent that may
// nest is judged by the agents it can reach (nestsWriter) and a remote agent that shares this host by
// its being there (remoteSharesHost). Plan mode asks without it: it refuses each call at its own depth
// (planModeRefusal, ExecutionContext.PlanGuarded), so a child that only nests stays a reader there.
func (s *subagentTool) callWrites(arguments map[string]any, below bool) bool {
	task, _ := arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		return false
	}
	if reference, _ := arguments["agent"].(string); strings.HasPrefix(reference, remote.Prefix) {
		// A remote agent changes the files of its own server, which is another machine unless the entry
		// is a socket or a loopback address (remoteSharesHost).
		return below && s.remoteSharesHost(reference)
	}
	agent, problem := s.selectAgent(s.withResumeAgent(arguments))
	if problem != "" {
		return false
	}
	// Scheduling follows the sandbox the call runs in: a writer agent called in a sandbox that
	// writes only scratch is a reader call. A refused choice changes nothing here; the call
	// fails before it starts anything.
	if chosen, refused := s.chooseSandbox(agent, s.callSandboxName(arguments)); refused == nil {
		agent = chosen
	}
	return agent.changesWorkspace() || (below && s.nestsWriter(agent))
}

// nestsWriter reports whether a run of agent may start a run that changes the session's files: it can
// nest and some agent of the tool is a writer, or a remote that shares this host is allowed. Which
// agent a nested call picks is up to the child's model, so every agent counts, at every depth: a
// grandchild that nests again is judged by the same rule, so the answer does not depend on the depth.
// The agents are read under the tool's lock and the caller holds no lock, so nothing nests.
func (s *subagentTool) nestsWriter(agent subagentAgent) bool {
	if !agent.nests {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nestsWriterLocked(agent)
}

// nestsWriterLocked is nestsWriter for a caller that holds s.mu (read or write), which takes no lock.
func (s *subagentTool) nestsWriterLocked(agent subagentAgent) bool {
	if !agent.nests {
		return false
	}
	for _, other := range s.agents {
		if other.changesWorkspace() {
			return true
		}
	}
	return len(s.sameHostRemotes()) > 0
}

// sameHostRemotes are the allowed remotes whose server may change this host's files (remoteSharesHost).
func (s *subagentTool) sameHostRemotes() []string {
	var names []string
	for _, name := range s.remotes.names {
		if s.remoteSharesHost(remote.Prefix + name) {
			names = append(names, name)
		}
	}
	return names
}

// remoteSharesHost reports whether a remote agent may change this host's files, by the snapshot of its
// entry the tool took when it was built (remoteSnapshot), so a call is judged the same way when it is
// scheduled, checkpointed and started, whatever happens to the entry file in between. The remote runs in
// the working folder of its own profile, which this host cannot see, so it is a path on this machine
// when the entry is a unix socket or a loopback address, whose server can be given the session's folder.
// An entry that could not be read then, or a remote the tool does not know, counts as sharing: the call
// fails to start, and serializing it costs nothing.
//
// This is a heuristic about the address and not confinement (remote.Entry.SameHost): a name that resolves
// to this machine, this machine's own LAN or public address, a tunnel, or a network file system shared
// with a server at another address is not detected and reads as another machine, whose calls are not
// checkpointed here; a remote's own session on its side is never under this host's checkpoint.
func (s *subagentTool) remoteSharesHost(reference string) bool {
	name, _, _ := remote.Split(reference)
	snapshot, known := s.remotes.snapshot[name]
	return !known || snapshot.sameHost
}

// withResumeAgent is the call's arguments with agent set to the agent of the run a resume
// continues, when the call names none: the run is started with that agent (startRunFrom), so
// it is judged by it. A run that is not known leaves the arguments as they are; the resume
// refuses it before anything starts.
func (s *subagentTool) withResumeAgent(arguments map[string]any) map[string]any {
	if name, _ := arguments["agent"].(string); name != "" {
		return arguments
	}
	resume, _ := arguments["resume"].(string)
	if resume == "" || s.manager == nil {
		return arguments
	}
	if run := s.manager.byID(resume); run != nil {
		return withAgent(arguments, run.view().agent)
	}
	return arguments
}

// planModeRefusal is the tool's Tool.PlanModeRefusal: why plan mode does not run a call, or "". A
// call that may change files is refused (callWrites). So is a call to a remote agent, which
// runs on another JAD server that is not told this run is in plan mode, so nothing there keeps it
// from changing files: this refuses the start here and does not confine the remote host. The same
// check runs for the calls of a nested child (ExecutionContext.PlanGuarded, execute), so no depth
// widens what plan mode allows.
func (s *subagentTool) planModeRefusal(arguments map[string]any, _ *tools.ExecutionContext) string {
	task, _ := arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		return ""
	}
	if agent, _ := arguments["agent"].(string); strings.HasPrefix(agent, remote.Prefix) {
		return "Plan mode: a remote agent runs on another server that is not told this turn is plan mode, so it is not started in this turn. Use a local read-only agent, or put the remote work in the plan."
	}
	if s.callWrites(arguments, false) {
		return tools.PlanModeChangeRefusal
	}
	return ""
}

// subagentListText is the text appended to the manifest description: the agent list, or,
// when there is no agent yet, how to create one.
func subagentListText(settings sessionfs.NativeSubagentSettings, lines []agentLine) string {
	if len(settings.Agents) == 0 {
		return "\n" + noAgentsText(settings)
	}
	return renderAgentLines(lines, settings.ListMaxBytes)
}

// noAgentsText tells the model there is no agent and what to create. The folders are
// read when a message starts a run, so a new agent is usable from the next message.
func noAgentsText(settings sessionfs.NativeSubagentSettings) string {
	if len(settings.DenyAgents) > 0 {
		return "All agents are disabled by tools.native.subagent.deny_agents; none can be launched."
	}
	dirs := settings.ConfiguredAgentDirs
	where := "define one in tools.native.subagent.agents"
	if len(dirs) > 0 {
		where = "create a folder named after the agent in " + strings.Join(dirs, " or ")
	}
	return "No agents are configured. To add one, " + where + ", holding agent.json (optional keys: description, model, tools, instructions) and/or prompt.md; the name is 1 to 64 letters, digits, underscores or hyphens. It is available from the next message."
}

// subagentAgentList returns the text appended to the manifest description for agents marked by
// their config alone (changeMarker; background names the agents that may be handed off). The tool
// itself marks agents by how it classifies them (agentMarker).
func subagentAgentList(agents []sessionfs.NativeSubagentAgent, maxBytes int64, background map[string]bool) string {
	lines := make([]agentLine, len(agents))
	for index, agent := range agents {
		lines[index] = agentLine{name: agent.Name, marker: changeMarker(agent, background[agent.Name]), description: agent.Description}
	}
	return renderAgentLines(lines, maxBytes)
}

// agentLine is one agent of the list: its name, the marker that says what it may change, and its
// description.
type agentLine struct {
	name, marker, description string
}

// renderAgentLines is the agent list. The list is paid for on every request, so it shrinks with its
// size: names and descriptions while they fit in maxBytes, else names alone, else a count; the last
// two say how to get the rest. The count line is always sent, whatever the budget, so the model
// always knows the tool lists agents when asked.
func renderAgentLines(agents []agentLine, maxBytes int64) string {
	var full strings.Builder
	full.WriteString("\nAgents:")
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		full.WriteString("\n- ")
		full.WriteString(agent.name)
		full.WriteString(agent.marker)
		if agent.description != "" {
			full.WriteString(": ")
			full.WriteString(agent.description)
		}
		names = append(names, agent.name)
	}
	if int64(full.Len()) <= maxBytes {
		return full.String()
	}
	const askForList = " Call without a task to list them with descriptions."
	if inline := "\nAgents: " + strings.Join(names, ", ") + "." + askForList; int64(len(inline)) <= maxBytes {
		return inline
	}
	noun := "agents"
	if len(agents) == 1 {
		noun = "agent"
	}
	return fmt.Sprintf("\n%d %s configured.%s", len(agents), noun, askForList)
}

// changeMarker tells the model that a call to this agent can change files, and whether it
// runs to its end or may be handed off like any other run.
func changeMarker(agent sessionfs.NativeSubagentAgent, background bool) string {
	switch {
	case !agent.ChangesFiles():
		return ""
	case agent.Isolated():
		return " [can change files in a git worktree of its own; its changes come back as a branch]"
	case background:
		return " [can change files; may run in the background]"
	}
	return " [can change files; runs to its end]"
}

// renderSubagentList is the result of a call with no task: one line per agent. The runs
// are listed by the async tool.
func renderSubagentList(callID string, agents []subagentAgent, maxBytes int64, settings sessionfs.NativeSubagentSettings, mark func(subagentAgent) string) (string, error) {
	lines := make([]string, 0, len(agents)+1)
	if len(agents) == 0 {
		lines = append(lines, noAgentsText(settings))
	}
	for _, agent := range agents {
		line := agent.config.Name + mark(agent)
		if agent.config.Description != "" {
			line += ": " + oneLine(agent.config.Description)
		}
		lines = append(lines, line)
	}
	text, cut := cutUTF8(strings.Join(lines, "\n"), maxBytes)
	information := []tools.Information{{Key: "agents", Value: len(agents)}}
	if cut {
		information = append(information, tools.Information{Key: "truncated", Value: true})
	}
	plural := "s"
	if len(agents) == 1 {
		plural = ""
	}
	return tools.RenderToolResult(tools.ResultOptions{
		ID:          callID,
		Status:      tools.StatusOK,
		Summary:     fmt.Sprintf("%d agent%s available", len(agents), plural),
		Information: information,
		Content:     &tools.Content{Encoding: "utf8", Text: text},
	})
}

// newSubagentAgent builds one agent from the native tools it may be offered. Its own settings
// narrow the pool exactly as a profile's tools block narrows the registered tools: the agent's
// allow list (when it restricts), its deny list and the session profile's tools.deny. Nothing else
// does: a child has every native tool of the pool, not only the read-only ones. An allow entry that
// names no tool of the pool is an error that lists what is enabled, so a typo is not a quiet gap.
func newSubagentAgent(agent sessionfs.NativeSubagentAgent, sessionDeny []string, pool []tools.Tool, nesting bool) (subagentAgent, error) {
	filter := agent.ToolSettings(sessionDeny)
	available := make([]string, 0, len(pool))
	byName := make(map[string]struct{}, len(pool))
	for _, tool := range pool {
		available = append(available, tool.Definition.Name)
		byName[tool.Definition.Name] = struct{}{}
	}
	sort.Strings(available)
	mcpListed := false
	for _, name := range filter.Allow {
		if strings.HasPrefix(name, mcpToolIDPrefix) {
			mcpListed = true
			continue
		}
		// The two tools the owner builds for each child: subagent (nestedSelf) and load_tool (SetLazy).
		if name == "subagent" || name == sessionfs.LoadToolName {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		switch name {
		case "tool_smith":
			return subagentAgent{}, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"tool_smith\", but the session does not enable tools.native.tool_smith: add it under tools.native, or remove it from the list", agent.Name)
		case "profile_message":
			return subagentAgent{}, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"profile_message\", but the session does not enable tools.native.profile_message (or has no other profile to message): add it under tools.native, or remove it from the list", agent.Name)
		case "async":
			return subagentAgent{}, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"async\", but the session does not enable tools.native.async: add \"async\": {} under tools.native, or remove it from the list", agent.Name)
		}
		return subagentAgent{}, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, which is not an enabled native tool (enabled: %s); an MCP tool is written mcp:<server>:<tool>", agent.Name, name, strings.Join(available, ", "))
	}
	shown := make([]tools.Tool, 0, len(pool))
	for _, tool := range pool {
		if tools.ProfileShows(filter, tool.Definition) {
			shown = append(shown, tool)
		}
	}
	// A child with no tool that can change files is a reader: it is scheduled as one and may run as a handle.
	// Nesting does not make it a writer here, which is about its own tools; whether a child it may start
	// makes it one for scheduling and the checkpoint is the tool's to say once every agent is known
	// (nestsWriter), because no run of a child has a checkpoint of its own.
	selfShown := nesting && tools.ProfileShows(filter, tools.Definition{ID: subagentToolID, Source: "native", Name: "subagent"})
	changes, messages := false, false
	for _, tool := range shown {
		if sessionfs.IsChangeTool(tool.Definition.Name) {
			changes = true
		}
		if tool.Definition.Name == "profile_message" {
			messages = true
		}
	}
	agent.NoChangeTools, agent.NoProfileMessage = !changes, !messages
	if filter.HasAllow && len(shown) == 0 && !mcpListed && !selfShown {
		return subagentAgent{}, fmt.Errorf("tools.native.subagent.agents.%s has no tool to use: its tools list and the profile's tools.deny leave none of the session's tools (enabled: %s)", agent.Name, strings.Join(available, ", "))
	}
	return subagentAgent{config: agent, filter: filter, nativeTools: shown, needsMCP: mcpListed}, nil
}

// bindMCP gives every agent the MCP tools it may use and builds its catalog. An agent sees the
// MCP tools its own settings show, the way a profile's tools block shows the registered ones: all of
// the session's (or its toolset's) MCP tools, narrowed by the agent's allow and deny lists and the
// session profile's tools.deny.
//
// Each child's copy is the raw tool, gated by the owner's lazy set (the gate wraps the tool it is
// given, so a toolset's copy stays the toolset's) and then wrapped by hook: with the owner's hooks
// for a session tool, with the toolset's own hooks (ToolsetMCP.Hooks) for a toolset tool. The hooks
// wrap the gate, so a refused call to a lazy tool still fires them once.
func (s *subagentTool) bindMCP(mcpTools []tools.Tool, toolsetTools map[string]ToolsetMCP, ownerHooks sessionfs.HooksSettings, hook func(sessionfs.HooksSettings, tools.Tool) tools.Tool) error {
	session := newMCPPool(mcpTools)
	pools := make(map[string]mcpPool, len(toolsetTools))
	toolsetHooks := make(map[string]sessionfs.HooksSettings, len(toolsetTools))
	for id, loaded := range toolsetTools {
		pools[id] = newMCPPool(loaded.Tools)
		toolsetHooks[id] = loaded.Hooks
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.agents {
		agent := &s.agents[index]
		pool, hooks, where := session, ownerHooks, ""
		if toolsetKey := agent.config.ToolsetKey(); toolsetKey != "" {
			hooks = toolsetHooks[ToolsetMCPKey(agent.config)]
			pool = pools[ToolsetMCPKey(agent.config)]
			if pool.byID == nil {
				pool = mcpPool{byID: map[string]tools.Tool{}, loadedText: "none"}
			}
			where = " of toolset " + toolsetKey
		}
		if err := checkMCPEntries(agent, pool, where); err != nil {
			return err
		}
		agent.mcpTools = nil
		for _, tool := range pool.ordered {
			if !tools.ProfileShows(agent.filter, tool.Definition) {
				continue
			}
			// The owning profile's lazy set gates the child's copy: a lazy tool is never in the child's
			// tools array, and runs only through the child's load_tool (run). GateFor leaves the set's
			// one-shot gate to the parent.
			gated, _ := s.lazy.GateFor(tool)
			agent.mcpTools = append(agent.mcpTools, hook(hooks, gated))
		}
		agent.mcpBound = true
		// Bound MCP tools can make an isolated agent a writer (changesWorkspace).
		agent.background = backgroundSafe(s.settings, *agent, s.async)
	}
	// The tool is described again now that its agents' MCP tools are known (nestedSelf, which the
	// catalogs below hold, carries it); the owner's catalog is told through Redescribe.
	s.self.Definition.Description = s.description()
	for index := range s.agents {
		agent := &s.agents[index]
		if err := s.checkAgentHasTools(agent); err != nil {
			return err
		}
		if err := s.buildCatalogs(agent); err != nil {
			return err
		}
	}
	return nil
}

// checkAgentHasTools refuses an agent that has no tool at all once the MCP tools are bound: its own lists
// and the profile's tools.deny leave nothing, so a run of it could do nothing but answer. The message says
// when the deny list is in effect, so the cause is not mistaken for a typo in the agent file.
func (s *subagentTool) checkAgentHasTools(agent *subagentAgent) error {
	if len(agent.nativeTools) > 0 || len(agent.mcpTools) > 0 {
		return nil
	}
	if s.settings.MaxDepth >= 2 && tools.ProfileShows(agent.filter, s.self.Definition) {
		return nil
	}
	if s.loadTool != nil && tools.ProfileShows(agent.filter, s.loadTool.Definition) {
		return nil
	}
	err := fmt.Errorf("tools.native.subagent.agents.%s has no tool to use: its tools lists and the profile's tools.deny leave none of the session's tools", agent.config.Name)
	if len(s.settings.DenyTools) > 0 {
		err = fmt.Errorf("%w (the profile's tools.deny is in effect: %s)", err, strings.Join(s.settings.DenyTools, ", "))
	}
	return err
}

// checkMCPEntries refuses an allow entry of the agent that names an MCP tool, or a server, the
// pool did not load, so a typo is a configuration error and not a tool that is quietly absent.
func checkMCPEntries(agent *subagentAgent, pool mcpPool, where string) error {
	for _, id := range agent.filter.Allow {
		if !strings.HasPrefix(id, mcpToolIDPrefix) {
			continue
		}
		if server, wildcard := strings.CutSuffix(id, ":*"); wildcard {
			if _, err := expandMCPWildcard(agent.config.Name, id, server, pool.byID); err != nil {
				return err
			}
			continue
		}
		if _, exists := pool.byID[id]; !exists {
			return fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, which is not a loaded MCP tool%s (loaded: %s)", agent.config.Name, id, where, pool.loadedText)
		}
	}
	return nil
}

// mcpPool is the MCP tools one source loaded (the session, or one toolset), by ID, with
// the sorted IDs ready for an error message.
type mcpPool struct {
	byID map[string]tools.Tool
	// ordered are the tools in the order the source loaded them, as a profile's catalog holds them.
	ordered    []tools.Tool
	loadedText string
}

func newMCPPool(mcpTools []tools.Tool) mcpPool {
	byID := make(map[string]tools.Tool, len(mcpTools))
	var ordered []tools.Tool
	for _, tool := range mcpTools {
		if tool.Definition.Source == "mcp" {
			byID[tool.Definition.ID] = tool
			ordered = append(ordered, tool)
		}
	}
	loaded := make([]string, 0, len(byID))
	for id := range byID {
		loaded = append(loaded, id)
	}
	sort.Strings(loaded)
	loadedText := "none"
	if len(loaded) > 0 {
		loadedText = strings.Join(loaded, ", ")
	}
	return mcpPool{byID: byID, ordered: ordered, loadedText: loadedText}
}

// expandMCPWildcard returns the loaded tools of one server, by ID order, for an entry
// written mcp:<server>:*. The server prefix is the profile's matching rule. A wildcard
// that matches no tool is an error that names the server.
func expandMCPWildcard(agent, entry, serverPrefix string, byID map[string]tools.Tool) ([]tools.Tool, error) {
	var matched []tools.Tool
	for id, tool := range byID {
		if !strings.HasPrefix(id, serverPrefix+":") {
			continue
		}
		matched = append(matched, tool)
	}
	server := strings.TrimPrefix(serverPrefix, mcpToolIDPrefix)
	if len(matched) == 0 {
		return nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, but server %q has no loaded tool (is it enabled in mcp_servers and running?)", agent, entry, server)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Definition.ID < matched[j].Definition.ID })
	return matched, nil
}

// normalizeSubagentArguments treats a blank agent, resume or task as absent, so a model
// that sends an empty string for "the default" is not rejected, and a blank task as a
// request for the agent list. A negative block is no block.
func normalizeSubagentArguments(arguments map[string]any) map[string]any {
	for _, key := range []string{"task", "agent", "resume", "model", "sandbox"} {
		value, ok := arguments[key].(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed == "" {
			delete(arguments, key)
		} else if key != "task" {
			arguments[key] = trimmed
		}
	}
	if block, ok := safeInteger(arguments["block_ms"]); ok && block < 0 {
		arguments["block_ms"] = json.Number("0")
	}
	return arguments
}

func (s *subagentTool) selectAgent(arguments map[string]any) (subagentAgent, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.agents) == 0 {
		return subagentAgent{}, noAgentsText(s.settings)
	}
	name, named := arguments["agent"].(string)
	if !named {
		if len(s.agents) == 1 {
			return s.agents[0], ""
		}
		return subagentAgent{}, "Several agents are configured; name one in agent."
	}
	for _, agent := range s.agents {
		if agent.config.Name == name {
			return agent, ""
		}
	}
	return subagentAgent{}, fmt.Sprintf("No agent named %q.", name)
}

func (s *subagentTool) agentNames() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.agents))
	for _, agent := range s.agents {
		names = append(names, agent.config.Name)
	}
	return strings.Join(names, ", ")
}

// subagentReasonBytes bounds an error text copied into a result, so a provider error
// body cannot fill the parent's context.
const subagentReasonBytes = 400

func boundedReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) <= subagentReasonBytes {
		return reason
	}
	cut, _ := cutUTF8(reason, subagentReasonBytes)
	return cut + "..."
}
