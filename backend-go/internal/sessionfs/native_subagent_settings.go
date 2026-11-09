package sessionfs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultNativeSubagentResultMaxBytes int64 = 32 * 1024
	defaultNativeSubagentMaxConcurrent  int64 = 3
	// defaultNativeSubagentListMaxBytes keeps a handful of agents with one-line
	// descriptions inline in the tool description and moves a large set behind a call.
	defaultNativeSubagentListMaxBytes  int64 = 4096
	maxNativeSubagentListMaxBytes      int64 = 1024 * 1024
	maxNativeSubagentConcurrent        int64 = 32
	defaultNativeSubagentStorageDir          = ".subagents"
	defaultNativeSubagentMaxToolRounds int64 = 30
	defaultNativeSubagentTimeoutMS     int64 = 600_000
	maxNativeSubagentTimeoutMS         int64 = maxToolTimeoutMs
	defaultNativeSubagentKeepDone      int64 = 50
	maxNativeSubagentKeepDone          int64 = 1000
	defaultNativeSubagentMaxQueued     int64 = 32
	// defaultNativeSubagentChangedFilesMax is how many changed files a run lists in its
	// result; a child that changes more says how many it left out.
	defaultNativeSubagentChangedFilesMax int64 = 100
	maxNativeSubagentChangedFilesMax     int64 = 10_000
	maxNativeSubagentMaxQueued           int64 = 1024
	// defaultNativeSubagentMaxBackgroundWriters is how many writer runs one session may
	// have going as handles at once; a start beyond it waits for its end instead.
	defaultNativeSubagentMaxBackgroundWriters int64 = 1
	// Nesting: how deep a child may start children (1 = never), and how many runs one
	// top-level run and one session may have going at once through such starts.
	defaultNativeSubagentMaxDepth          int64 = 2
	maxNativeSubagentMaxDepth              int64 = 8
	defaultNativeSubagentMaxRunsPerRoot    int64 = 16
	defaultNativeSubagentMaxRunsPerSession int64 = 32
	maxNativeSubagentMaxRuns               int64 = 1024
	// The state-context section lists the session's runs after a compaction, so the
	// model still knows the ids it was given.
	defaultNativeSubagentStateLimit int64 = 20
	maxNativeSubagentStateLimit     int64 = 100_000
	// maxNativeSubagentDescriptionChars bounds the text each agent adds to the tool
	// description, which is sent to the model on every request.
	maxNativeSubagentDescriptionChars = 500
)

var (
	nativeSubagentKeys = []string{"agents", "agents_dir", "agents_dir_missing_ok", "agents_dir_override", "result_max_bytes", "max_concurrent", "list_max_bytes", "keep_finished", "max_queued", "persist_runs", "delete_transcripts", "deny_agents", "changed_files_max", "background_writers", "max_background_writers", "max_depth", "max_runs_per_root", "max_runs_per_session", "storage_dir", "statecontext", "remotes", "remote_timeout_ms", "default_block_ms", "worktree", "model_choice", "sandbox_choice"}
	// defaultNativeSubagentStateNames are the runs a model may still be waiting for.
	defaultNativeSubagentStateNames = []string{"queued", "running"}
	allowedNativeSubagentStates     = map[string]struct{}{
		"queued": {}, "running": {}, "done": {}, "failed": {}, "timed_out": {}, "stopped": {}, "lost": {},
	}
	nativeSubagentAgentKeys = []string{"description", "model", "tools", "toolset", "instructions", "prompt_documents", "max_tool_rounds", "max_run_tokens", "timeout_ms", "auto_compaction", "compaction_trigger_tokens", "state_context", "isolation", "sandbox"}
	subagentNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// NativeSubagentSettings contains the normalized settings of native.subagent.
type NativeSubagentSettings struct {
	// ModelChoice (model_choice, off by default) gives the tool a model argument and lists the
	// calling profile's models in its description. Off, the tool's definition is the one it had
	// before the model argument existed, and the loader leaves ProfileModels nil.
	ModelChoice bool
	// ProfileModels maps each model ID the calling profile lists to its file: the models a
	// call's model argument may pick. The catalog-layout loader fills it when ModelChoice is
	// on; nil offers none.
	ProfileModels map[string]string
	// SandboxChoice (sandbox_choice, off by default) are the sandbox templates a call's sandbox
	// argument may pick; SandboxChoices are them resolved by the catalog-layout loader. Nil
	// offers none, and the tool's definition is the one it had before the argument existed.
	SandboxChoice  []string
	SandboxChoices []SandboxChoice `json:"-"`
	// ResultMaxBytes is the most child text one call returns; a longer answer is cut
	// and the result says so.
	ResultMaxBytes int64
	// Remotes are the remote entries (remotes folder) whose agents the model may start with
	// agent "remote:<name>/<session>/<category>/<profile>/<agent>"; none by default.
	Remotes []string
	// RemoteTimeoutMs bounds a remote sub-agent run from the local side: past it the run is
	// stopped on the remote and ends as timed_out.
	RemoteTimeoutMs int64
	// DefaultBlockMs is how long a call that sends no block_ms waits for the answer before
	// it returns a run id, clamped to tools.native.async max_block_ms. Zero (settings built
	// in code) means the async default_block_ms.
	// A call that later calls of its response wait behind (ExecutionContext.BatchWaiting) waits
	// the async default_block_ms instead.
	DefaultBlockMs int64
	// MaxConcurrent is the most children that run at once. Children outlive a call, so
	// the limit counts every child of the same storage directory, across runs; the
	// session that first uses the directory sets it.
	MaxConcurrent int64
	// KeepFinished is how many finished runs stay readable by id; the oldest go first.
	// Runs still going are never dropped.
	KeepFinished int64
	// MaxQueued is how many runs may wait for a free slot while MaxConcurrent are
	// going. A start beyond it is refused. Zero allows no waiting at all.
	MaxQueued int64
	// ChangedFilesMax is the most files a run lists as changed (exec's changes are found by
	// the hooks.shell_changes scan); zero turns the list off.
	ChangedFilesMax int64
	// BackgroundWriters lets an agent that can change files run as an async handle (it
	// honours block_ms). Off by default: a writer then runs to its end, as before.
	BackgroundWriters bool
	// MaxBackgroundWriters is the most writer runs one session may have going as handles
	// at once; a writer started beyond it runs to its end instead. Zero, only possible in
	// a hand-built struct, means no limit.
	MaxBackgroundWriters int64
	// MaxDepth is how many levels of runs may exist below the session: 1 lets only the
	// session start children, 2 lets a child start a child of its own. Zero, only possible
	// in a hand-built struct, means 1.
	MaxDepth int64
	// MaxRunsPerRoot is the most runs below one top-level run that may be going at once,
	// and MaxRunsPerSession the most runs of one session (any depth) that a nested start
	// may find going. Zero, only possible in a hand-built struct, means no limit. Neither
	// is visible to the model.
	MaxRunsPerRoot    int64
	MaxRunsPerSession int64
	// PersistRuns writes a small manifest per run, so run ids and finished results
	// survive a server restart. A run that was going at the restart is reported as
	// lost. The zero value of this struct means "memory only"; the loader sets true.
	PersistRuns bool
	// DeleteTranscripts deletes a child's transcript folder from the storage directory
	// when keep_finished drops its run, so the folders stop growing. Off by default:
	// the transcript is the record of what a child did, and nothing deletes it unless
	// asked. The zero value is the default.
	DeleteTranscripts bool
	// DenyAgents are agent names removed from Agents after loading; a name that matches no agent is ignored.
	DenyAgents []string
	// DenyTools is the selected profile's tools.deny, set when the catalog is built and
	// not read from the subagent section: a tool it covers is in no child's catalog.
	DenyTools []string
	// ListMaxBytes is the most bytes of agent list the tool description may carry:
	// the full list when it fits, else the names, else a count. Zero never lists
	// inline, so the model has to ask. The zero value of this struct therefore means
	// "ask", not "default"; the loader always sets it.
	ListMaxBytes int64
	// StorageDir holds one transcript directory per child run.
	StorageDir string
	// Worktree configures the git worktrees of the agents with isolation worktree.
	Worktree NativeSubagentWorktreeSettings
	// StateContext bounds the run records copied into a compaction state-context
	// snapshot when statecontext.tools lists subagent.
	StateContext NativeSubagentStateContextSettings
	// AgentDirs are the absolute agents_dir directories that exist, in configured order.
	AgentDirs []string
	// ConfiguredAgentDirs are all agents_dir directories in configured order, including
	// those agents_dir_missing_ok skipped: the places a new agent folder can go.
	ConfiguredAgentDirs []string
	// Agents are the configured children: the inline ones in file order, then each
	// directory's folders in name order.
	Agents []NativeSubagentAgent
}

// NativeSubagentStateContextSettings selects which of the session's runs a
// state-context snapshot lists, and how many.
type NativeSubagentStateContextSettings struct {
	States []string
	Limit  int64
	// Mode is "items" (default) or "summary": a count per state, with the runs of
	// DetailStates still listed in full (at most Limit of them).
	Mode         string
	DetailStates []string
}

// agentChangeTools are the native tools that let a child change files or run code: exec and
// tool_smith run code, and subagent and profile_message start runs that may change files. They
// are not a restriction on what a child may use (every tool of the session is offered to it);
// they tell a restricted agent's tools list apart from a reading one (ChangesFiles).
var agentChangeTools = []string{"exec", "tool_smith", "subagent", "profile_message"}

// IsChangeTool reports whether the named native tool can change files or run code, so an agent that
// has it can change files.
func IsChangeTool(name string) bool { return containsString(agentChangeTools, name) }

// deprecatedAgentTools are the names an agent's tools list needed to opt in to a tool the
// default child set left out. Every tool is in a child's set now, so a list that names only
// these restricts nothing (ToolsUnrestricted).
var deprecatedAgentTools = []string{"async", "exec", "tool_smith", "subagent", "profile_message", LoadToolName}

// NativeSubagentAgent is one configured child agent.
type NativeSubagentAgent struct {
	Name        string
	Description string
	// ModelPath is the absolute model file; empty means the calling session's model.
	ModelPath string
	// ReasoningEffort is the effort written after "@" in a model ID; empty keeps the model file's own.
	ReasoningEffort string
	// Model is the `model` value as written. A catalog-layout session reads a value that
	// does not end in .json as a model ID and replaces ModelPath with that model's file
	// (resolveSubagentModels); a legacy session only uses ModelPath.
	Model        string
	Instructions string
	// PromptDocuments are the absolute paths of the files read into PromptText.
	PromptDocuments []string
	// PromptText is the child's whole system prompt: Instructions first, then
	// prompt.md of an agent folder, then each document in order, joined by a blank
	// line. Empty means the child gets none.
	PromptText string
	// Dir is the agent's folder when it was loaded from agents_dir; empty for an
	// agent defined inline in setting.json.
	Dir string
	// Tools is the agent's own allow list, written like a profile's tools.allow: native
	// tool names, mcp:<server>:<tool> and mcp:<server>:*. HasTools is false when the key
	// is absent, which means every tool the session registers (minus the session profile's
	// tools.deny), exactly as a profile without a tools block. A list naming only
	// deprecatedAgentTools restricts nothing either (ToolsUnrestricted).
	Tools    []string
	HasTools bool
	// DenyTools is the agent's own deny list (tools.deny of the object form of the tools
	// key), in the same entry form; it wins over Tools. Left out of JSON when empty, so the
	// fingerprint of an agent without it is what it was before the key existed.
	DenyTools []string `json:",omitempty"`
	// ToolsObject is true when the tools key is the object form (allow and deny written like a profile's
	// block). Its allow list is taken as written; only an array of the old opt-in names restricts nothing.
	ToolsObject bool `json:",omitempty"`
	// Toolset lists the "<category>/<name>" of the toolset files this agent draws its tools
	// from instead of the session's own toolset, first one winning a clash; empty means
	// the session's.
	Toolset       []string
	MaxToolRounds int64
	// MaxRunTokens is the most tokens the child may use; zero means no cap.
	MaxRunTokens int64
	TimeoutMS    int64
	// HasAutoCompaction is true when auto_compaction is written; AutoCompaction is its
	// value. Absent, the child follows the session's compaction.auto_enabled, or is on
	// when CompactionTriggerTokens is set.
	AutoCompaction    bool
	HasAutoCompaction bool
	// CompactionTriggerTokens is the input size, in tokens, at which this agent's child
	// compacts its own transcript. Zero means the session's compaction.trigger_tokens.
	CompactionTriggerTokens int64
	// StateContext sends the session's newest stored state-context block to the child.
	StateContext bool
	// Isolation is SubagentIsolationWorktree for an agent whose runs work in a git worktree
	// of their own, and empty otherwise. It is left out of JSON when empty, so the
	// fingerprint of an agent without it is what it was before the key existed.
	Isolation string `json:",omitempty"`
	// IsolationBase is sandbox.isolation.base of the agent's own sandbox: what its worktree
	// starts from (SandboxIsolationBaseWorking; empty is HEAD). Left out of JSON when empty.
	IsolationBase string `json:",omitempty"`
	// Sandbox is the agent's own sandbox (the agent key sandbox: a template name, a template
	// with overrides or an object), narrowed by the session's (NarrowSandbox); nil runs the
	// agent in its caller's. Left out of JSON when nil, so the fingerprint of an agent without
	// it is what it was before the key existed, while an edited template changes it.
	Sandbox *SandboxSettings `json:",omitempty"`
	// sandboxRaw is the sandbox value as written, expanded and narrowed once the session's
	// sandbox and template folder are known (resolveAgentSandboxes); isolationKey says the
	// agent key isolation was written, which sandbox.isolation may not repeat.
	sandboxRaw   json.RawMessage
	isolationKey bool
	// NoChangeTools is set by the subagent tool when it builds the agent's catalog and finds no
	// tool in it that can change files (exec, tool_smith, profile_message, or subagent when it may
	// nest): the agent is then a reader, whatever its settings allowed. It is not read from a file.
	NoChangeTools bool `json:",omitempty"`
	// NoProfileMessage is set with NoChangeTools when the agent's catalog has no
	// profile_message: such a child reaches no other session, so an enforcing sandbox that
	// writes only scratch makes it a reader (SandboxReader). Not part of the fingerprint.
	NoProfileMessage bool `json:"-"`
}

// Agent returns the configured agent with the given name.
func (s NativeSubagentSettings) Agent(name string) (NativeSubagentAgent, bool) {
	for _, agent := range s.Agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return NativeSubagentAgent{}, false
}

func normalizeNativeSubagentSettings(setting map[string]json.RawMessage, settingPath, cwd, toolStorageDir string) (*NativeSubagentSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["subagent"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.subagent"
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if key == "allow_write" {
			return nil, configError("%s: %s.allow_write was removed; %s", settingPath, section, removedAllowWriteHint)
		}
		if key == "process_max_running" {
			return nil, configError("%s: %s.process_max_running was removed; %s", settingPath, section, removedProcessMaxRunningHint)
		}
		if !containsString(nativeSubagentKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeSubagentKeys, "")
		}
	}
	settings := &NativeSubagentSettings{}
	if settings.ResultMaxBytes, err = configuredInteger(fields, "result_max_bytes", defaultNativeSubagentResultMaxBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.Remotes, err = normalizeRemoteNames(fields["remotes"], settingPath, section); err != nil {
		return nil, err
	}
	if settings.RemoteTimeoutMs, err = configuredInteger(fields, "remote_timeout_ms", defaultNativeSubagentTimeoutMS, 1000, maxNativeSubagentTimeoutMS, settingPath, section); err != nil {
		return nil, err
	}
	if settings.DefaultBlockMs, err = configuredInteger(fields, "default_block_ms", DefaultNativeAgentBlockMs, 1, maxToolTimeoutMs, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxConcurrent, err = configuredInteger(fields, "max_concurrent", defaultNativeSubagentMaxConcurrent, 1, maxNativeSubagentConcurrent, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListMaxBytes, err = configuredInteger(fields, "list_max_bytes", defaultNativeSubagentListMaxBytes, 0, maxNativeSubagentListMaxBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.KeepFinished, err = configuredInteger(fields, "keep_finished", defaultNativeSubagentKeepDone, 1, maxNativeSubagentKeepDone, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxQueued, err = configuredInteger(fields, "max_queued", defaultNativeSubagentMaxQueued, 0, maxNativeSubagentMaxQueued, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ChangedFilesMax, err = configuredInteger(fields, "changed_files_max", defaultNativeSubagentChangedFilesMax, 0, maxNativeSubagentChangedFilesMax, settingPath, section); err != nil {
		return nil, err
	}
	if settings.BackgroundWriters, err = bashBool(fields, "background_writers", false, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.ModelChoice, err = bashBool(fields, "model_choice", false, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.SandboxChoice, err = normalizeSandboxChoiceNames(fields["sandbox_choice"], settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxBackgroundWriters, err = configuredInteger(fields, "max_background_writers", defaultNativeSubagentMaxBackgroundWriters, 1, maxNativeSubagentConcurrent, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxDepth, err = configuredInteger(fields, "max_depth", defaultNativeSubagentMaxDepth, 1, maxNativeSubagentMaxDepth, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxRunsPerRoot, err = configuredInteger(fields, "max_runs_per_root", defaultNativeSubagentMaxRunsPerRoot, 1, maxNativeSubagentMaxRuns, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxRunsPerSession, err = configuredInteger(fields, "max_runs_per_session", defaultNativeSubagentMaxRunsPerSession, 1, maxNativeSubagentMaxRuns, settingPath, section); err != nil {
		return nil, err
	}
	if settings.PersistRuns, err = bashBool(fields, "persist_runs", true, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.DeleteTranscripts, err = bashBool(fields, "delete_transcripts", false, settingPath+": "+section); err != nil {
		return nil, err
	}
	if settings.DenyAgents, err = subagentDenyAgents(fields, settingPath, section); err != nil {
		return nil, err
	}
	if settings.StorageDir, err = normalizeNativeSubagentStorageDir(fields, cwd, toolStorageDir, settingPath); err != nil {
		return nil, err
	}
	if settings.StateContext, err = normalizeNativeSubagentStateContext(fields["statecontext"], settingPath); err != nil {
		return nil, err
	}
	if settings.Worktree, err = normalizeNativeSubagentWorktree(fields["worktree"], cwd, settingPath); err != nil {
		return nil, err
	}
	agentsRaw, hasAgents := fields["agents"]
	hasAgents = hasAgents && strings.TrimSpace(string(agentsRaw)) != "null"
	missingOK, err := bashBool(fields, "agents_dir_missing_ok", false, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	override, err := bashBool(fields, "agents_dir_override", false, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	dirsRaw := fields["agents_dir"]
	hasDirsKey := len(dirsRaw) > 0 && strings.TrimSpace(string(dirsRaw)) != "null"
	if settings.AgentDirs, settings.ConfiguredAgentDirs, err = normalizeSubagentAgentDirs(fields, cwd, settingPath, missingOK); err != nil {
		return nil, err
	}
	// Zero agents is a valid state (a new project the model fills itself), but a section
	// that names neither source is a mistake.
	if !hasAgents && !hasDirsKey {
		return nil, configError("%s: %s needs agents or agents_dir; an empty object or list is allowed when there is no agent yet", settingPath, section)
	}
	taken := make(map[string]string)
	if hasAgents {
		names, err := orderedObjectKeys(agentsRaw)
		if err != nil {
			return nil, configError("%s: %s.agents must be an object", settingPath, section)
		}
		agentFields, err := decodeConfigObject(agentsRaw, settingPath+": "+section+".agents")
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !subagentNamePattern.MatchString(name) {
				return nil, configError("%s: %s.agents.%s: the name must be 1 to 64 letters, digits, underscores or hyphens", settingPath, section, name)
			}
			agent, err := normalizeNativeSubagentAgent(name, agentFields[name], settingPath, filepath.Dir(settingPath), cwd)
			if err != nil {
				return nil, err
			}
			taken[name] = settingPath + " (" + section + ".agents)"
			settings.Agents = append(settings.Agents, agent)
		}
	}
	inline := len(settings.Agents)
	for _, dir := range settings.AgentDirs {
		agents, err := loadSubagentDirAgents(dir, settingPath, taken, override)
		if err != nil {
			return nil, err
		}
		settings.Agents = withSubagentOverrides(settings.Agents, agents, inline)
	}
	settings.Agents = withoutDeniedSubagents(settings.Agents, settings.DenyAgents)
	return settings, nil
}

func normalizeNativeSubagentStateContext(raw json.RawMessage, settingPath string) (NativeSubagentStateContextSettings, error) {
	settings := NativeSubagentStateContextSettings{States: append([]string(nil), defaultNativeSubagentStateNames...), Limit: defaultNativeSubagentStateLimit, Mode: NativeStateContextModeItems}
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	const section = "tools.native.subagent.statecontext"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return NativeSubagentStateContextSettings{}, configError("%s: %s must be an object", settingPath, section)
	}
	for key := range fields {
		if key != "states" && key != "limit" && key != "mode" && key != "detail_states" {
			return NativeSubagentStateContextSettings{}, unsupportedToolKey(settingPath+": "+section, key, []string{"states", "limit", "mode", "detail_states"}, "")
		}
	}
	if value, exists := fields["states"]; exists && strings.TrimSpace(string(value)) != "null" {
		var names []string
		if err := json.Unmarshal(value, &names); err != nil || names == nil {
			return NativeSubagentStateContextSettings{}, configError("%s: %s.states must contain run states", settingPath, section)
		}
		states := make([]string, 0, len(names))
		seen := make(map[string]struct{}, len(names))
		for _, name := range names {
			if _, ok := allowedNativeSubagentStates[name]; !ok {
				return NativeSubagentStateContextSettings{}, configError("%s: %s.states must contain run states (queued, running, done, failed, timed_out, stopped)", settingPath, section)
			}
			if _, duplicate := seen[name]; !duplicate {
				states = append(states, name)
				seen[name] = struct{}{}
			}
		}
		settings.States = states
	}
	if settings.Limit, err = configuredInteger(fields, "limit", defaultNativeSubagentStateLimit, 1, maxNativeSubagentStateLimit, settingPath, section); err != nil {
		return NativeSubagentStateContextSettings{}, err
	}
	where := settingPath + ": " + section
	if settings.Mode, err = normalizeNativeStateContextMode(fields, where); err != nil {
		return NativeSubagentStateContextSettings{}, err
	}
	if settings.DetailStates, err = normalizeNativeStateContextDetail(fields, "detail_states", where, settings.Mode, settings.States, defaultNativeSubagentStateNames); err != nil {
		return NativeSubagentStateContextSettings{}, err
	}
	return settings, nil
}

func normalizeNativeSubagentStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	const field = "tools.native.subagent.storage_dir"
	dir, err := normalizeToolStorageSubdir(fields, defaultNativeSubagentStorageDir, cwd, toolStorageDir, field, settingPath)
	var problem *ConfigError
	if err != nil && !errors.As(err, &problem) {
		// Keep the config error of a path that cannot be made absolute, so it still maps to 422.
		return "", configError("%s: %s could not be resolved: %v", settingPath, field, errors.Unwrap(err))
	}
	return dir, err
}

func normalizeNativeSubagentAgent(name string, raw json.RawMessage, settingPath, settingDir, cwd string) (NativeSubagentAgent, error) {
	section := "tools.native.subagent.agents." + name
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return NativeSubagentAgent{}, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return NativeSubagentAgent{}, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if key == "paths" {
			return NativeSubagentAgent{}, configError("%s: %s.paths was removed; %s", settingPath, section, removedAgentPathsHint)
		}
		if key == "read_only" {
			return NativeSubagentAgent{}, configError("%s: %s.read_only was removed; %s", settingPath, section, removedAgentReadOnlyHint)
		}
		if !containsString(nativeSubagentAgentKeys, key) {
			return NativeSubagentAgent{}, unsupportedToolKey(settingPath+": "+section, key, nativeSubagentAgentKeys, "")
		}
	}
	return buildNativeSubagentAgent(name, fields, subagentSource{path: settingPath, section: section, modelBase: settingDir, documentBase: cwd}, "", "")
}

// subagentSource says where an agent was defined, for error messages and for
// resolving the relative paths it names.
type subagentSource struct {
	// path and section start every error: "<path>: <section>.<key> ...".
	path    string
	section string
	// modelBase and documentBase are what a relative model and a relative
	// prompt_documents entry resolve from.
	modelBase    string
	documentBase string
}

// buildNativeSubagentAgent turns already checked fields into an agent. promptFile is
// the text of an agent folder's prompt.md (empty for an inline agent) and
// promptFilePath its path.
func buildNativeSubagentAgent(name string, fields map[string]json.RawMessage, source subagentSource, promptFile, promptFilePath string) (NativeSubagentAgent, error) {
	settingPath, section, settingDir, cwd := source.path, source.section, source.modelBase, source.documentBase
	var err error
	agent := NativeSubagentAgent{Name: name}
	if agent.Description, err = optionalSubagentString(fields, "description", settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if len([]rune(agent.Description)) > maxNativeSubagentDescriptionChars {
		return NativeSubagentAgent{}, configError("%s: %s.description must be at most %d characters", settingPath, section, maxNativeSubagentDescriptionChars)
	}
	model, err := optionalSubagentString(fields, "model", settingPath, section)
	if err != nil {
		return NativeSubagentAgent{}, err
	}
	agent.Model = model
	if model != "" {
		agent.ModelPath = resolveFrom(settingDir, expandHome(model))
	}
	if agent.Instructions, err = optionalSubagentString(fields, "instructions", settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.PromptDocuments, agent.PromptText, err = loadSubagentPrompt(fields, []string{agent.Instructions, promptFile}, cwd, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if strings.TrimSpace(promptFile) != "" {
		agent.PromptDocuments = append([]string{promptFilePath}, agent.PromptDocuments...)
	}
	if agent.Tools, agent.HasTools, agent.DenyTools, agent.ToolsObject, err = optionalSubagentTools(fields, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.Toolset, err = optionalSubagentToolsets(fields, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.MaxToolRounds, err = configuredInteger(fields, "max_tool_rounds", defaultNativeSubagentMaxToolRounds, 1, maxConfiguredToolRounds, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.MaxRunTokens, err = configuredInteger(fields, "max_run_tokens", 0, 1, maxConfiguredRunTokens, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultNativeSubagentTimeoutMS, 1000, maxNativeSubagentTimeoutMS, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.AutoCompaction, agent.HasAutoCompaction, err = optionalSubagentBool(fields, "auto_compaction", settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.CompactionTriggerTokens, err = configuredInteger(fields, "compaction_trigger_tokens", 0, 1, maxCompactionTriggerTokens, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.HasAutoCompaction && !agent.AutoCompaction && agent.CompactionTriggerTokens > 0 {
		return NativeSubagentAgent{}, configError("%s: %s.compaction_trigger_tokens has no effect when auto_compaction is false", settingPath, section)
	}
	if agent.StateContext, _, err = optionalSubagentBool(fields, "state_context", settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if agent.Isolation, err = subagentIsolation(fields, agent, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
	}
	if raw, has := fields["isolation"]; has && strings.TrimSpace(string(raw)) != "null" {
		agent.isolationKey = true
	}
	if raw, has := fields["sandbox"]; has && strings.TrimSpace(string(raw)) != "null" {
		agent.sandboxRaw = raw
	}
	return agent, nil
}

// loadSubagentPrompt reads the agent's prompt_documents the way the session reads its
// own: a relative path is taken from the session's cwd and ~ is expanded. The files
// are read when the settings load, so a missing file fails the session at once and
// not in the middle of a child run. The prompt is the leading texts (the instructions,
// then an agent folder's prompt.md) and then each document, joined by a blank line.
func loadSubagentPrompt(fields map[string]json.RawMessage, leading []string, cwd, settingPath, section string) ([]string, string, error) {
	var paths []string
	if raw, exists := fields["prompt_documents"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &paths); err != nil || paths == nil {
			return nil, "", configError("%s: %s.prompt_documents must be an array of file paths", settingPath, section)
		}
	}
	parts := make([]string, 0, len(paths)+len(leading))
	for _, text := range leading {
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	resolvedPaths := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for index, document := range paths {
		if strings.TrimSpace(document) == "" {
			return nil, "", configError("%s: %s.prompt_documents[%d] must be a non-empty string", settingPath, section, index)
		}
		resolved := expandHome(strings.TrimSpace(document))
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(cwd, resolved)
		}
		resolved = filepath.Clean(resolved)
		if _, duplicate := seen[resolved]; duplicate {
			return nil, "", configError("%s: %s.prompt_documents lists %s twice", settingPath, section, resolved)
		}
		seen[resolved] = struct{}{}
		contents, err := readConfigTextFile(resolved)
		if err != nil {
			return nil, "", configError("%s: %s.prompt_documents[%d] could not be read: %s: %v", settingPath, section, index, resolved, err)
		}
		parts = append(parts, string(contents))
		resolvedPaths = append(resolvedPaths, resolved)
	}
	return resolvedPaths, strings.Join(parts, "\n\n"), nil
}

// optionalSubagentBool reads a boolean that may be absent or null; has reports whether
// a value was written.
func optionalSubagentBool(fields map[string]json.RawMessage, name, settingPath, section string) (value, has bool, err error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return false, false, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false, configError("%s: %s.%s must be a boolean", settingPath, section, name)
	}
	return value, true, nil
}

// optionalSubagentString reads a string that may be absent or null; a present
// value must not be blank.
func optionalSubagentString(fields map[string]json.RawMessage, name, settingPath, section string) (string, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", configError("%s: %s.%s must be a non-empty string", settingPath, section, name)
	}
	return strings.TrimSpace(value), nil
}

// optionalSubagentToolsets reads the toolset key: a string or an array, trimmed; absent or null is none.
func optionalSubagentToolsets(fields map[string]json.RawMessage, settingPath, section string) ([]string, error) {
	raw, exists := fields["toolset"]
	if !exists {
		return nil, nil
	}
	ids, problem := parseToolsetRefs(raw, true)
	if problem != "" {
		return nil, configError("%s: %s.toolset %s", settingPath, section, problem)
	}
	return ids, nil
}

// optionalSubagentTools reads an agent's tools key: an array of tool names (the allow list),
// or an object with allow and deny arrays, written like a profile's tools block. It returns
// the allow list, whether there is one, and the deny list.
func optionalSubagentTools(fields map[string]json.RawMessage, settingPath, section string) (allow []string, hasAllow bool, deny []string, object bool, err error) {
	raw, exists := fields["tools"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, false, nil, false, nil
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		settings, err := normalizeProfileTools(raw, settingPath+": "+section)
		if err != nil {
			return nil, false, nil, false, err
		}
		return settings.Allow, settings.HasAllow, settings.Deny, true, nil
	}
	names, hasAllow, err := subagentToolNames(raw, settingPath, section)
	return names, hasAllow, nil, false, err
}

func subagentToolNames(raw json.RawMessage, settingPath, section string) ([]string, bool, error) {
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || names == nil {
		return nil, false, configError("%s: %s.tools must be an array of tool names, or an object with allow and deny", settingPath, section)
	}
	seen := make(map[string]struct{}, len(names))
	for index, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, false, configError("%s: %s.tools[%d] must be a non-empty string", settingPath, section, index)
		}
		if _, removed := removedNativeTools[name]; removed || strings.HasPrefix(name, ProfileToolMCPPrefix) {
			if err := checkProfileToolEntry(name); err != nil {
				return nil, false, configError("%s: %s.tools[%d] %v", settingPath, section, index, err)
			}
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, false, configError("%s: %s.tools lists %s twice", settingPath, section, name)
		}
		seen[name] = struct{}{}
		names[index] = name
	}
	return names, true, nil
}

// ChangeTools returns the tools of the agent's list that can change files or run code,
// in list order.
func (a NativeSubagentAgent) ChangeTools() []string {
	var names []string
	for _, name := range a.Tools {
		if containsString(agentChangeTools, name) {
			names = append(names, name)
		}
	}
	return names
}

// ToolsUnrestricted reports whether the agent's own tools list leaves the whole catalog in
// place: no list, or an array that names only the deprecated opt-in tools (async, exec, tool_smith,
// subagent, profile_message, load_tool), which a child has by default now. An empty list is a
// list: it allows nothing, as an empty tools.allow does; so is an allow of the object form.
func (a NativeSubagentAgent) ToolsUnrestricted() bool {
	if !a.HasTools {
		return true
	}
	if len(a.Tools) == 0 || a.ToolsObject {
		return false
	}
	for _, name := range a.Tools {
		if !containsString(deprecatedAgentTools, name) {
			return false
		}
	}
	return true
}

// ToolsListDeprecated reports whether the agent's tools list is one that names only the
// deprecated opt-in tools, which the loader ignores (and warns about).
func (a NativeSubagentAgent) ToolsListDeprecated() bool {
	return a.HasTools && len(a.Tools) > 0 && !a.ToolsObject && a.ToolsUnrestricted()
}

// ToolSettings is the agent's own narrowing as a profile's tools block: its allow list when it
// restricts, and its deny list and the session profile's tools.deny (sessionDeny). The child
// catalog is the registered one shown by it, the way applyProfileTools shows a profile's.
func (a NativeSubagentAgent) ToolSettings(sessionDeny []string) ProfileToolSettings {
	settings := ProfileToolSettings{Restricted: true}
	if !a.ToolsUnrestricted() {
		settings.HasAllow, settings.Allow = true, a.Tools
	}
	settings.Deny = append(append([]string(nil), sessionDeny...), a.DenyTools...)
	return settings
}

// ChangesFiles reports whether the agent can change files: it can when it has no tools list;
// with a list, when the list names a tool that can. Once the subagent tool has built the
// agent's catalog, NoChangeTools says there is no such tool in it. There is no read-only mode:
// whether the files a run changes are the session's own is isolation's to say (ChangesWorkspace).
func (a NativeSubagentAgent) ChangesFiles() bool {
	if a.NoChangeTools {
		return false
	}
	if a.ToolsUnrestricted() {
		return true
	}
	return len(a.ChangeTools()) > 0
}

// ToolsetKeySeparator joins the toolset IDs of an agent into one key. An ID never holds
// it, and the order of the IDs is their precedence, so [a, b] and [b, a] are two keys.
const ToolsetKeySeparator = "+"

// ToolsetKey is the key of the agent's toolsets in SessionConfig.AgentToolsets: the ID
// itself for one toolset, the IDs joined for several, empty for none.
func (a NativeSubagentAgent) ToolsetKey() string {
	return strings.Join(a.Toolset, ToolsetKeySeparator)
}
