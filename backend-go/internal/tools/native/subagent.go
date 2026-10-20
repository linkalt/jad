package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const subagentToolID = "native.subagent"

var subagentInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "task":{"type":"string","minLength":1,"description":"Self-contained instructions. The agent sees nothing of this conversation. Omit to list the agents."},
    "agent":{"type":"string","description":"Agent name. Optional when only one is configured."},
    "id":{"type":"string","minLength":1,"description":"Run id from a call that returned still running. Use it instead of task."},
    "wait_ms":{"type":"integer","minimum":0,"description":"Most time to wait for the answer; 0 returns at once with a run id. Clamped to session policy. An agent that can change files ignores it unless the session allows detached writers."},
    "stop":{"type":"boolean","description":"With id: cancel that run."}
  },
  "additionalProperties":false
}`)

const subagentDescription = "Hand a task to a separate agent with its own fresh context and get back only its final answer. Agents only read unless marked [can change files]. Use it for searches or reading that would fill your context. A slow agent returns a run id while it keeps working; never start the task again, call with that id. A marked agent is never handed off: the call runs to its end."

// subagentExample is shown to the model when its arguments are rejected.
var subagentExample = json.RawMessage(`{"task":"Find where the retry delay is computed and report the file paths and line numbers."}`)

// SubagentRequest is one child run: the agent to start, its task, and the tools it may call.
type SubagentRequest struct {
	Agent sessionfs.NativeSubagentAgent
	Task  string
	// CallID is the parent's tool call, used to name the child's transcript.
	CallID string
	// Tools is the child's catalog: read-only native tools only, never the subagent tool.
	Tools *tools.Catalog
	// Execution carries the workspace roots the child's tools resolve paths against.
	Execution tools.ExecutionContext
	// OnTranscript, when set, is called once with the child's transcript directory as
	// soon as the launcher has created it, so the manager knows where the child's work
	// is even if the server dies before the child ends.
	OnTranscript func(dir string)
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
}

// Bind gives each agent that lists MCP tools its catalog. It fails when a listed tool
// is not among the loaded ones or belongs to a server that asks for approval, because
// a child has no way to ask. It does nothing when the session has no subagent tool.
func (m *SubagentMCP) Bind(mcpTools []tools.Tool) error {
	return m.BindToolsets(mcpTools, nil)
}

// BindToolsets is Bind plus the MCP tools loaded for each toolset an agent names, by
// toolset ID. An agent with a toolset resolves its mcp: entries against that toolset's
// tools only, never the session's.
func (m *SubagentMCP) BindToolsets(mcpTools []tools.Tool, toolsetTools map[string][]tools.Tool) error {
	if m == nil || m.tool == nil {
		return nil
	}
	return m.tool.bindMCP(mcpTools, toolsetTools)
}

type subagentTool struct {
	settings sessionfs.NativeSubagentSettings
	// mu guards the agents' catalogs, which Bind fills in after the tool is built.
	mu       sync.RWMutex
	agents   []subagentAgent
	launcher SubagentLauncher
	// manager owns the runs, so a run outlives the call that started it.
	manager *SubagentManager
}

// NewSubagentTool builds the subagent tool over the already built tools of the same
// catalog. A child may use only the read-only ones, optionally narrowed by its
// agent's tools list, and never the subagent tool itself, so the depth is one.
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

// subagentToolset is the native tools built from one toolset file an agent names, and the
// process manager they use. The parent's model never sees them.
type subagentToolset struct {
	native    []tools.Tool
	processes ProcessManager
}

// childToolPools splits tools into the read-only ones and, when allow_write is on, the
// ones that change files, leaving out the subagent tool and what the profile's
// tools.deny covers. Only native tools are taken.
func childToolPools(settings sessionfs.NativeSubagentSettings, available []tools.Tool) (readOnly, changers []tools.Tool) {
	for _, tool := range available {
		if (tool.Definition.Source != "native" && tool.Definition.Source != "extension") || tool.Definition.ID == subagentToolID || tool.Definition.ID == toolSmithToolID || tools.AnyEntryMatches(settings.DenyTools, tool.Definition) {
			continue
		}
		switch {
		case tool.ReadOnly:
			readOnly = append(readOnly, tool)
		case settings.AllowWrite && containsName(sessionfs.SubagentChangeTools, tool.Definition.Name):
			changers = append(changers, tool)
		}
	}
	return readOnly, changers
}

func newSubagentTool(settings sessionfs.NativeSubagentSettings, parentTools []tools.Tool, toolsets map[string]subagentToolset, launcher SubagentLauncher, mcp *SubagentMCP, manager *SubagentManager, processes ProcessManager) (tools.Tool, error) {
	validator, err := tools.CompileSchema(subagentInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native subagent schema: %w", err)
	}
	if settings.MaxConcurrent < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.max_concurrent must be at least 1")
	}
	if settings.MaxWaitMS < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.max_wait_ms must be at least 1")
	}
	if settings.KeepFinished < 1 {
		return tools.Tool{}, errors.New("tools.native.subagent.keep_finished must be at least 1")
	}
	if manager == nil {
		manager = NewSubagentManager()
	}
	readOnly, changers := childToolPools(settings, parentTools)
	runtime := &subagentTool{settings: settings, launcher: launcher, manager: manager}
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
			agentProcesses = toolset.processes
		}
		childTools, mcpIDs, err := subagentChildTools(agent, agentReadOnly, agentChangers, settings.AllowWrite)
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
		runtime.agents = append(runtime.agents, entry)
	}
	if mcp != nil {
		mcp.tool = runtime
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          subagentToolID,
			Source:      "native",
			Name:        "subagent",
			Description: subagentDescription + subagentListText(settings),
			InputSchema: append(json.RawMessage(nil), subagentInputSchema...),
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
	}, nil
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
// can when the agent the call picks lists a tool that changes files. A call that picks
// no agent, or only checks a run, cannot.
func (s *subagentTool) callMayChangeFiles(arguments map[string]any) bool {
	if id, _ := arguments["id"].(string); id != "" {
		return false
	}
	task, _ := arguments["task"].(string)
	if strings.TrimSpace(task) == "" {
		return false
	}
	agent, problem := s.selectAgent(arguments)
	return problem == "" && agent.config.ChangesFiles()
}

// subagentListText is the text appended to the manifest description: the agent list, or,
// when there is no agent yet, how to create one.
func subagentListText(settings sessionfs.NativeSubagentSettings) string {
	if len(settings.Agents) == 0 {
		return "\n" + noAgentsText(settings)
	}
	return subagentAgentList(settings.Agents, settings.ListMaxBytes)
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
func subagentAgentList(agents []sessionfs.NativeSubagentAgent, maxBytes int64) string {
	var full strings.Builder
	full.WriteString("\nAgents:")
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		full.WriteString("\n- ")
		full.WriteString(agent.Name)
		full.WriteString(changeMarker(agent))
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

// changeMarker tells the model that a call to this agent can change files, so it runs
// to its end instead of handing back a run id.
func changeMarker(agent sessionfs.NativeSubagentAgent) string {
	if agent.ChangesFiles() {
		return " [can change files; runs to its end]"
	}
	return ""
}

// renderSubagentList is the result of a call with no task: one line per agent, then
// one per retained run, so a model that lost a run id can find it again.
func renderSubagentList(callID string, agents []subagentAgent, runs []subagentRunView, maxBytes int64, settings sessionfs.NativeSubagentSettings) (string, error) {
	lines := make([]string, 0, len(agents)+len(runs)+2)
	if len(agents) == 0 {
		lines = append(lines, noAgentsText(settings))
	}
	for _, agent := range agents {
		line := agent.config.Name + changeMarker(agent.config)
		if agent.config.Description != "" {
			line += ": " + oneLine(agent.config.Description)
		}
		lines = append(lines, line)
	}
	if len(runs) > 0 {
		lines = append(lines, "", "Runs:")
		for _, run := range runs {
			lines = append(lines, fmt.Sprintf("%s %s %s %ds: %s", run.id, run.agent, run.state, int64(run.elapsed.Seconds()), run.task))
		}
	}
	text, cut := cutUTF8(strings.Join(lines, "\n"), maxBytes)
	information := []tools.Information{{Key: "agents", Value: len(agents)}}
	if len(runs) > 0 {
		information = append(information, tools.Information{Key: "runs", Value: len(runs)})
	}
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
func subagentChildTools(agent sessionfs.NativeSubagentAgent, readOnly, changers []tools.Tool, allowWrite bool) ([]tools.Tool, []string, error) {
	available := make([]string, 0, len(readOnly)+len(changers))
	byName := make(map[string]tools.Tool, len(readOnly)+len(changers))
	for _, tool := range append(append([]tools.Tool(nil), readOnly...), changers...) {
		available = append(available, tool.Definition.Name)
		byName[tool.Definition.Name] = tool
	}
	sort.Strings(available)
	kind := "read-only native tool"
	if allowWrite {
		kind = "native tool"
	}
	chosen := readOnly
	var mcpIDs []string
	if agent.HasTools {
		wanted := make(map[string]struct{}, len(agent.Tools))
		for _, name := range agent.Tools {
			if strings.HasPrefix(name, mcpToolIDPrefix) {
				mcpIDs = append(mcpIDs, name)
				continue
			}
			if _, exists := byName[name]; !exists {
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
	if len(chosen) == 0 && len(mcpIDs) == 0 {
		return nil, nil, fmt.Errorf("tools.native.subagent.agents.%s has no tool to use: enable at least one read-only native tool (read, grep, glob, fetch, skill or decide)", agent.Name)
	}
	return chosen, mcpIDs, nil
}

// bindMCP builds the catalog of every agent that lists MCP tools. Only the tools an
// agent names are given; a tool is never added because its server calls it read-only,
// since that is the server's own claim.
func (s *subagentTool) bindMCP(mcpTools []tools.Tool, toolsetTools map[string][]tools.Tool) error {
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
		if toolsetKey := agent.config.ToolsetKey(); toolsetKey != "" {
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
				childTools = append(childTools, tool)
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
			if tool.Approval == tools.ApprovalAsk {
				return fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, whose server asks for approval; a sub-agent has no way to ask the user, so the call would always be blocked", agent.config.Name, id)
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
// written mcp:<server>:*. The server prefix is the profile's matching rule. A tool whose
// server asks for approval is skipped, since the child cannot ask; a wildcard that is
// left with no tool is an error that names the server.
func expandMCPWildcard(agent, entry, serverPrefix string, byID map[string]tools.Tool) ([]tools.Tool, error) {
	var matched []tools.Tool
	skipped := 0
	for id, tool := range byID {
		if !strings.HasPrefix(id, serverPrefix+":") {
			continue
		}
		if tool.Approval == tools.ApprovalAsk {
			skipped++
			continue
		}
		matched = append(matched, tool)
	}
	server := strings.TrimPrefix(serverPrefix, mcpToolIDPrefix)
	if len(matched) == 0 {
		if skipped > 0 {
			return nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, but server %q asks for approval; a sub-agent has no way to ask the user, so none of its tools can be used", agent, entry, server)
		}
		return nil, fmt.Errorf("tools.native.subagent.agents.%s.tools lists %q, but server %q has no loaded tool (is it enabled in mcp_servers and running?)", agent, entry, server)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Definition.ID < matched[j].Definition.ID })
	return matched, nil
}

// normalizeSubagentArguments treats a blank agent, id or task as absent, so a model
// that sends an empty string for "the default" is not rejected, and a blank task as a
// request for the agent list. A negative wait is no wait.
func normalizeSubagentArguments(arguments map[string]any) map[string]any {
	for _, key := range []string{"task", "agent", "id"} {
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
	if wait, ok := safeInteger(arguments["wait_ms"]); ok && wait < 0 {
		arguments["wait_ms"] = json.Number("0")
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
