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

var subagentInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "task":{"type":"string","minLength":1,"description":"Self-contained instructions. The agent sees nothing of this conversation. Omit to list the agents."},
    "agent":{"type":"string","description":"Agent name. Optional when only one is configured."},` + subagentModelProperty + `
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
	// background is set when a call to this agent may run as an async handle although
	// the agent can change files; see backgroundSafe.
	background bool
}

// backgroundSafe reports whether a writer agent may run as a handle: the session turned
// background_writers on, the async tool is there to collect the run, and the agent
// has a tool that changes the session's files. An agent with isolation worktree is
// handed off like a reader without it.
func backgroundSafe(settings sessionfs.NativeSubagentSettings, agent subagentAgent, host *AsyncHost) bool {
	if !settings.BackgroundWriters || host == nil || !agent.config.ChangesWorkspace() {
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
	schema := modelChoiceSchema(subagentInputSchema, subagentModelProperty, settings.ModelChoice)
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
		runtime.remotes = remoteAgentSettings{names: settings.Remotes, directory: remotes, timeout: time.Duration(settings.RemoteTimeoutMs) * time.Millisecond}
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
			Description: subagentDescription + subagentListText(runtime.resolvedSettings(), runtime.backgroundNames()) + remoteListText(runtime.remotes.names) + subagentModelText(settings),
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
		NormalizeArguments: normalizeSubagentArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return runtime.execute(ctx, arguments, execution)
		},
	}
	runtime.self = self
	if err := runtime.buildAllCatalogs(); err != nil {
		return tools.Tool{}, err
	}
	return self, nil
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
	if s.settings.MaxDepth >= 2 && tools.ProfileShows(agent.filter, s.self.Definition) {
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

// callMayChangeFiles is the tool's per-call answer to "can this call change files": it
// can when the agent the call picks lists a tool that changes files and works in the
// session's own folder (an agent with isolation worktree changes only its worktree). A
// call that picks no agent, or only checks a run, cannot.
func (s *subagentTool) callMayChangeFiles(arguments map[string]any) bool {
	task, _ := arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		return false
	}
	if agent, _ := arguments["agent"].(string); strings.HasPrefix(agent, remote.Prefix) {
		// Whatever a remote agent changes, it changes on the remote machine.
		return false
	}
	agent, problem := s.selectAgent(arguments)
	return problem == "" && agent.config.ChangesWorkspace()
}

// subagentListText is the text appended to the manifest description: the agent list, or,
// when there is no agent yet, how to create one.
func subagentListText(settings sessionfs.NativeSubagentSettings, background map[string]bool) string {
	if len(settings.Agents) == 0 {
		return "\n" + noAgentsText(settings)
	}
	return subagentAgentList(settings.Agents, settings.ListMaxBytes, background)
}

// resolvedSettings is the tool's settings with each agent as it was built (NoChangeTools set), which is
// what the agent list shows the model.
func (s *subagentTool) resolvedSettings() sessionfs.NativeSubagentSettings {
	resolved := s.settings
	resolved.Agents = make([]sessionfs.NativeSubagentAgent, len(s.agents))
	for index, agent := range s.agents {
		resolved.Agents[index] = agent.config
	}
	return resolved
}

// backgroundNames are the agents whose calls may be handed off although they can change files.
func (s *subagentTool) backgroundNames() map[string]bool {
	names := make(map[string]bool)
	for _, agent := range s.agents {
		if agent.background {
			names[agent.config.Name] = true
		}
	}
	return names
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

// subagentAgentList returns the text appended to the manifest description. The agent
// list is paid for on every request, so it shrinks with its size: names and
// descriptions while they fit in maxBytes, else names alone, else a count; the last
// two say how to get the rest. The count line is always sent, whatever the budget, so
// the model always knows the tool lists agents when asked.
func subagentAgentList(agents []sessionfs.NativeSubagentAgent, maxBytes int64, background map[string]bool) string {
	var full strings.Builder
	full.WriteString("\nAgents:")
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		full.WriteString("\n- ")
		full.WriteString(agent.Name)
		full.WriteString(changeMarker(agent, background[agent.Name]))
		if agent.Description != "" {
			full.WriteString(": ")
			full.WriteString(agent.Description)
		}
		names = append(names, agent.Name)
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
func renderSubagentList(callID string, agents []subagentAgent, maxBytes int64, settings sessionfs.NativeSubagentSettings) (string, error) {
	lines := make([]string, 0, len(agents)+1)
	if len(agents) == 0 {
		lines = append(lines, noAgentsText(settings))
	}
	for _, agent := range agents {
		line := agent.config.Name + changeMarker(agent.config, agent.background)
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
	// Nesting alone does not make it a writer: a child it starts that changes files is a call of its own run,
	// which that run's checkpoint and change list cover.
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
	for _, key := range []string{"task", "agent", "resume", "model"} {
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
