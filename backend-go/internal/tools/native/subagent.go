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
	// Tools is the child's catalog: read-only native tools, plus subagent, tool_smith or profile_message when the agent lists them.
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
	// catalog is what the child may call. It is nil for an agent that lists MCP tools
	// until SubagentMCP.Bind has seen the loaded MCP tools.
	catalog *tools.Catalog
	// nativeTools and mcpIDs are the two halves of the agent's tools list; mcpIDs is
	// empty for an agent that uses native tools only.
	nativeTools []tools.Tool
	mcpIDs      []string
	// processes is the process manager a child that lists process starts its processes
	// in, under its run id; the run's end stops them. It is the session's, or the one of
	// the toolset the agent names, and nil when that source has no process tool.
	processes ProcessManager
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

// SetLazy gives the owning profile's lazy set to the children: each child's MCP tools and its nested
// subagent tool in that set are gated with it. loadTool is the child's load_tool, built by the owner from the
// same set and manifest; nil when the owner enables no load_tool. A child gets it only when its tools list names it.
func (m *SubagentMCP) SetLazy(lazy *tools.LazySet, loadTool *tools.Tool) error {
	if m == nil || m.tool == nil {
		return nil
	}
	m.tool.lazy = lazy
	m.tool.loadTool = loadTool
	if err := m.tool.regateNestedSelf(); err != nil {
		return err
	}
	if loadTool != nil {
		return m.tool.addLoadToolToNestingAgents()
	}
	return nil
}

// BindToolsets is Bind plus the MCP tools loaded for each toolset an agent names, by
// toolset ID. An agent with a toolset resolves its mcp: entries against that toolset's
// tools only, never the session's.
func (m *SubagentMCP) BindToolsets(mcpTools []tools.Tool, toolsetTools map[string][]tools.Tool) error {
	if m == nil || m.tool == nil {
		return nil
	}
	return m.tool.bindMCP(mcpTools, toolsetTools, m.hookTool)
}

// hookTool wraps a nested copy of a tool with the owning profile's hooks. A nil handle returns the
// tool unchanged, as newSubagentToolRemotes builds a tool without one.
func (m *SubagentMCP) hookTool(tool tools.Tool) tools.Tool {
	if m == nil {
		return tool
	}
	return WithToolEventHooks(tool, m.hooks, m.apiKeyEnv)
}

type subagentTool struct {
	// self is the tool as the owner built it, before any lazy gate or hook; nestedSelf makes each
	// nesting child's copy from it.
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
// catalog. A child may use the read-only ones, optionally narrowed by its agent's tools
// list; subagent, tool_smith and profile_message are there only for an agent that lists them, and the
// depth is bounded by max_depth.
func NewSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher) (tools.Tool, error) {
	return NewSubagentToolWithMCP(settings, parentTools, launcher, nil)
}

// NewSubagentToolWithMCP is NewSubagentTool plus the hook that later gives the tool
// the MCP tools an agent lists. A nil mcp is allowed; an agent that lists an MCP tool
// then cannot start a child.
func NewSubagentToolWithMCP(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher, mcp *SubagentMCP) (tools.Tool, error) {
	return NewSubagentToolWithManager(settings, parentTools, launcher, mcp, nil)
}

// NewSubagentToolWithManager is NewSubagentToolWithMCP plus the manager that owns the
// runs. The resolver passes one shared by every session of a storage directory and
// closes it at shutdown. A nil manager gives the tool a private one that nothing
// closes, which suits tests only.
func NewSubagentToolWithManager(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager) (tools.Tool, error) {
	return NewSubagentToolWithProcesses(settings, parentTools, launcher, mcp, manager, nil)
}

// NewSubagentToolWithProcesses is NewSubagentToolWithManager plus the session's process
// manager, which a child that lists the process tool needs: its processes are stopped
// when its run ends.
func NewSubagentToolWithProcesses(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, processes ProcessManager) (tools.Tool, error) {
	return newSubagentTool(settings, parentTools, nil, launcher, mcp, manager, processes)
}

func newSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, processes ProcessManager) (tools.Tool, error) {
	return newSubagentToolAsync(settings, parentTools, toolsets, launcher, mcp, manager, processes, nil)
}

// subagentToolset is the native tools built from one toolset file an agent names, and the
// process manager they use. The parent's model never sees them.
type subagentToolset struct {
	native    []tools.Tool
	processes ProcessManager
}

// childToolPools splits tools into the read-only ones and the ones that change files,
// leaving out what the profile's tools.deny covers. Only native tools are taken.
func childToolPools(settings sessionfs.NativeSubagentSettings, available []tools.Tool) (readOnly, changers []tools.Tool) {
	for _, tool := range available {
		if (tool.Definition.Source != "native" && tool.Definition.Source != "extension") || tools.AnyEntryMatches(settings.DenyTools, tool.Definition) {
			continue
		}
		switch {
		case tool.Definition.ID == profileMessageToolID:
			// Listed by name like async: the agent's tools list alone decides.
			readOnly = append(readOnly, tool)
		case tool.ReadOnly:
			readOnly = append(readOnly, tool)
		case containsName(sessionfs.SubagentChangeTools, tool.Definition.Name):
			changers = append(changers, tool)
		}
	}
	return readOnly, changers
}

// newSubagentToolAsync is newSubagentTool plus the async host that tracks every read-only run
// the tool starts as a handle owned by the calling session. A nil host starts no handles.
func newSubagentToolAsync(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, processes ProcessManager, host *AsyncHost) (tools.Tool, error) {
	return newSubagentToolRemotes(settings, parentTools, toolsets, launcher, mcp, manager, processes, host, nil)
}

// newSubagentToolRemotes is newSubagentToolAsync plus the remotes folder the session's
// tools.native.subagent.remotes names are read from.
func newSubagentToolRemotes(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, processes ProcessManager, host *AsyncHost, remotes *remote.Directory) (tools.Tool, error) {
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
	readOnly, changers := childToolPools(settings, parentTools)
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
		agent = withoutDeniedTools(agent, settings.DenyTools)
		agentReadOnly, agentChangers, agentProcesses := readOnly, changers, processes
		toolsetKey := agent.ToolsetKey()
		if toolsetKey != "" {
			toolset, built := toolsets[toolsetKey]
			if !built {
				return tools.Tool{}, fmt.Errorf("tools.native.subagent.agents.%s.toolset %q was not loaded", agent.Name, toolsetKey)
			}
			agentReadOnly, agentChangers = childToolPools(settings, toolset.native)
			// A toolset cannot set profile_message; the session's tool is offered to the agent that lists it.
			for _, tool := range readOnly {
				if tool.Definition.ID == profileMessageToolID {
					agentReadOnly = append(agentReadOnly[:len(agentReadOnly):len(agentReadOnly)], tool)
				}
			}
			agentProcesses = toolset.processes
		}
		childTools, mcpIDs, err := subagentChildTools(agent, agentReadOnly, agentChangers)
		if err != nil {
			if toolsetKey != "" {
				err = fmt.Errorf("%w (its tools come from toolset %s)", err, toolsetKey)
			}
			if len(settings.DenyTools) > 0 {
				err = fmt.Errorf("%w (the profile's tools.deny is in effect: %s)", err, strings.Join(settings.DenyTools, ", "))
			}
			return tools.Tool{}, err
		}
		entry := subagentAgent{config: agent, nativeTools: childTools, mcpIDs: mcpIDs, processes: agentProcesses}
		if len(mcpIDs) == 0 {
			catalog, err := tools.NewCatalog(childTools)
			if err != nil {
				return tools.Tool{}, fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.Name, err)
			}
			entry.catalog = catalog
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
			Description: subagentDescription + subagentListText(settings, runtime.backgroundNames()) + remoteListText(runtime.remotes.names) + subagentModelText(settings),
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
	if err := runtime.addSelfToNestingAgents(); err != nil {
		return tools.Tool{}, err
	}
	return self, nil
}

// addSelfToNestingAgents gives the subagent tool to the agents whose tools list names it,
// so such a child can start children of its own (bounded by max_depth and the run caps).
// An agent that lists MCP tools gets it when its catalog is built in bindMCP.
func (s *subagentTool) addSelfToNestingAgents() error {
	for index := range s.agents {
		agent := &s.agents[index]
		if !containsName(agent.config.Tools, "subagent") {
			continue
		}
		if s.settings.MaxDepth < 2 {
			return fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"subagent\", but tools.native.subagent.max_depth is %d: raise it to 2 or more, or remove it from the list", agent.config.Name, max(s.settings.MaxDepth, 1))
		}
		agent.nativeTools = append(agent.nativeTools, s.nestedSelf())
		if len(agent.mcpIDs) > 0 {
			continue
		}
		catalog, err := tools.NewCatalog(agent.nativeTools)
		if err != nil {
			return fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
		}
		agent.catalog = catalog
	}
	return nil
}

// nestedSelf is the subagent tool as a nesting child gets it: gated with the owner's lazy set when the set
// has bound it, and hooked with the owner's hooks. GateFor leaves the set's one-shot gate to the parent's copy.
func (s *subagentTool) nestedSelf() tools.Tool {
	gated, _ := s.lazy.GateFor(s.self)
	return s.mcp.hookTool(gated)
}

// regateNestedSelf replaces the nested copy of the subagent tool that each agent lists with one gated by the
// owner's lazy set, which is known only once SetLazy runs. An agent without MCP tools gets its catalog rebuilt;
// an agent with MCP tools gets its catalog in bindMCP, so SetLazy must run before BindToolsets.
func (s *subagentTool) regateNestedSelf() error {
	for index := range s.agents {
		agent := &s.agents[index]
		if !containsName(agent.config.Tools, "subagent") {
			continue
		}
		for position := range agent.nativeTools {
			if agent.nativeTools[position].Definition.ID == subagentToolID {
				agent.nativeTools[position] = s.nestedSelf()
			}
		}
		if len(agent.mcpIDs) > 0 {
			continue
		}
		catalog, err := tools.NewCatalog(agent.nativeTools)
		if err != nil {
			return fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
		}
		agent.catalog = catalog
	}
	return nil
}

// addLoadToolToNestingAgents gives load_tool to the agents whose tools list names it. A child with
// MCP tools gets it in bindMCP, which builds its catalog from nativeTools; a child without any gets
// its catalog rebuilt here.
func (s *subagentTool) addLoadToolToNestingAgents() error {
	for index := range s.agents {
		agent := &s.agents[index]
		if !containsName(agent.config.Tools, sessionfs.LoadToolName) {
			continue
		}
		agent.nativeTools = append(agent.nativeTools, *s.loadTool)
		if len(agent.mcpIDs) > 0 {
			continue
		}
		catalog, err := tools.NewCatalog(agent.nativeTools)
		if err != nil {
			return err
		}
		agent.catalog = catalog
	}
	return nil
}

// withoutDeniedTools drops from an agent's tools list every entry the profile's
// tools.deny covers, so a denied tool cannot be reached through a child. An agent
// without a tools list is returned as it is: its default set is filtered when built.
// A wildcard entry that covers only some tools is expanded in bindMCP.
func withoutDeniedTools(agent sessionfs.NativeSubagentAgent, deny []string) sessionfs.NativeSubagentAgent {
	if !agent.HasTools || len(deny) == 0 {
		return agent
	}
	kept := make([]string, 0, len(agent.Tools))
	for _, name := range agent.Tools {
		definition := tools.Definition{ID: name, Source: "native", Name: name}
		if strings.HasPrefix(name, mcpToolIDPrefix) {
			definition = tools.Definition{ID: name, Source: "mcp"}
		}
		if !tools.AnyEntryMatches(deny, definition) {
			kept = append(kept, name)
		}
	}
	agent.Tools = kept
	return agent
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

// subagentChildTools picks the native tools one agent may use from the enabled
// read-only ones, and returns separately the MCP tool IDs its list names. An agent
// without a tools list gets every native read-only tool and no MCP tool.
func subagentChildTools(agent sessionfs.NativeSubagentAgent, readOnly, changers []tools.Tool) ([]tools.Tool, []string, error) {

	available := make([]string, 0, len(readOnly)+len(changers))
	byName := make(map[string]tools.Tool, len(readOnly)+len(changers))
	for _, tool := range append(append([]tools.Tool(nil), readOnly...), changers...) {
		available = append(available, tool.Definition.Name)
		byName[tool.Definition.Name] = tool
	}
	sort.Strings(available)
	const kind = "native tool"
	// async, tool_smith, subagent, profile_message and load_tool are opt-in: a child gets them only when its tools list names them.
	chosen := make([]tools.Tool, 0, len(readOnly))
	for _, tool := range readOnly {
		if tool.Definition.ID != asyncToolID && tool.Definition.ID != toolSmithToolID && tool.Definition.ID != subagentToolID && tool.Definition.ID != profileMessageToolID && tool.Definition.ID != loadToolID {
			chosen = append(chosen, tool)
		}
	}
	var mcpIDs []string
	if agent.HasTools {
		wanted := make(map[string]struct{}, len(agent.Tools))
		for _, name := range agent.Tools {
			if strings.HasPrefix(name, mcpToolIDPrefix) {
				mcpIDs = append(mcpIDs, name)
				continue
			}
			if name == "subagent" {
				// The tool being built; the caller adds it to this agent's catalog.
				continue
			}
			// load_tool is offered by addLoadToolToNestingAgents, once the owning profile's set is known.
			if name == sessionfs.LoadToolName {
				continue
			}
			if _, exists := byName[name]; !exists {
				switch name {
				case "tool_smith":
					return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"tool_smith\", but the session does not enable tools.native.tool_smith: add it under tools.native, or remove it from the list", agent.Name)
				case "profile_message":
					return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"profile_message\", but the session does not enable tools.native.profile_message (or has no other profile to message): add it under tools.native, or remove it from the list", agent.Name)
				case "async":
					return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists \"async\", but the session does not enable tools.native.async: add \"async\": {} under tools.native, or remove it from the list", agent.Name)
				}
				return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, which is not an enabled %s (enabled: %s); an MCP tool is written mcp:<server>:<tool>", agent.Name, name, kind, strings.Join(available, ", "))
			}
			wanted[name] = struct{}{}
		}
		chosen = make([]tools.Tool, 0, len(wanted))
		for _, tool := range append(append([]tools.Tool(nil), readOnly...), changers...) {
			if _, ok := wanted[tool.Definition.Name]; ok {
				chosen = append(chosen, tool)
			}
		}
	}
	if len(chosen) == 0 && len(mcpIDs) == 0 && !containsName(agent.Tools, "subagent") {
		return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s has no tool to use: enable at least one read-only native tool (read, grep, glob, fetch, skill, decide_systemone or web_search)", agent.Name)
	}
	return chosen, mcpIDs, nil
}

// bindMCP builds the catalog of every agent that lists MCP tools. Only the tools an
// agent names are given; a tool is never added because its server calls it read-only,
// since that is the server's own claim.
//
// hook wraps a child's copy of a session MCP tool with the owner's hooks. It runs after
// the lazy gate, because GateFor rebuilds a lazy tool from the set. Toolset tools are not wrapped:
// the resolver already hooks them with their own profile's hooks.
func (s *subagentTool) bindMCP(mcpTools []tools.Tool, toolsetTools map[string][]tools.Tool, hook func(tools.Tool) tools.Tool) error {
	session := newMCPPool(mcpTools)
	pools := make(map[string]mcpPool, len(toolsetTools))
	for id, loadedTools := range toolsetTools {
		pools[id] = newMCPPool(loadedTools)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.agents {
		agent := &s.agents[index]
		if len(agent.mcpIDs) == 0 {
			continue
		}
		byID, loadedText := session.byID, session.loadedText
		where := ""
		fromToolset := false
		if toolsetKey := agent.config.ToolsetKey(); toolsetKey != "" {
			fromToolset = true
			pool := pools[toolsetKey]
			byID, loadedText = pool.byID, pool.loadedText
			if byID == nil {
				byID, loadedText = map[string]tools.Tool{}, "none"
			}
			where = " of toolset " + toolsetKey
		}
		childTools := append([]tools.Tool(nil), agent.nativeTools...)
		added := make(map[string]struct{}, len(agent.mcpIDs))
		add := func(tool tools.Tool) {
			if tools.AnyEntryMatches(s.settings.DenyTools, tool.Definition) {
				return
			}
			if _, done := added[tool.Definition.ID]; !done {
				added[tool.Definition.ID] = struct{}{}
				// The owning profile's lazy set gates the child's copy: a lazy tool stays off the child's
				// request until the child loads it. GateFor leaves the set's one-shot gate to the parent.
				gated, _ := s.lazy.GateFor(tool)
				if !fromToolset {
					gated = hook(gated)
				}
				childTools = append(childTools, gated)
			}
		}
		for _, id := range agent.mcpIDs {
			if server, wildcard := strings.CutSuffix(id, ":*"); wildcard {
				matched, err := expandMCPWildcard(agent.config.Name, id, server, byID)
				if err != nil {
					return err
				}
				for _, tool := range matched {
					add(tool)
				}
				continue
			}
			tool, exists := byID[id]
			if !exists {
				return fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, which is not a loaded MCP tool%s (loaded: %s)", agent.config.Name, id, where, loadedText)
			}
			add(tool)
		}
		catalog, err := tools.NewCatalog(childTools)
		if err != nil {
			return fmt.Errorf("tools.native.subagent.agents.%s: %w", agent.config.Name, err)
		}
		agent.catalog = catalog
	}
	return nil
}

// mcpPool is the MCP tools one source loaded (the session, or one toolset), by ID, with
// the sorted IDs ready for an error message.
type mcpPool struct {
	byID       map[string]tools.Tool
	loadedText string
}

func newMCPPool(mcpTools []tools.Tool) mcpPool {
	byID := make(map[string]tools.Tool, len(mcpTools))
	for _, tool := range mcpTools {
		if tool.Definition.Source == "mcp" {
			byID[tool.Definition.ID] = tool
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
	return mcpPool{byID: byID, loadedText: loadedText}
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
