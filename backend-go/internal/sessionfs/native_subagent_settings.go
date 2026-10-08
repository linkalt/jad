package sessionfs

import (
	"encoding/json"
	"os"
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
	maxNativeSubagentTimeoutMS         int64 = 3_600_000
	// A call waits wait_ms for the child's answer and then returns the run's id with the
	// child still going, so a slow child is not thrown away; timeout_ms stays the hard
	// limit on the child's whole life. max_wait_ms caps any wait the model asks for.
	defaultNativeSubagentWaitMS    int64 = 120_000
	defaultNativeSubagentMaxWaitMS int64 = 600_000
	maxNativeSubagentWaitMS        int64 = 3_600_000
	defaultNativeSubagentKeepDone  int64 = 50
	maxNativeSubagentKeepDone      int64 = 1000
	defaultNativeSubagentMaxQueued int64 = 32
	maxNativeSubagentMaxQueued     int64 = 1024
	// The state-context section lists the session's runs after a compaction, so the
	// model still knows the ids it was given.
	defaultNativeSubagentStateLimit int64 = 20
	maxNativeSubagentStateLimit     int64 = 100_000
	// maxNativeSubagentDescriptionChars bounds the text each agent adds to the tool
	// description, which is sent to the model on every request.
	maxNativeSubagentDescriptionChars = 500
)

var (
	nativeSubagentKeys = []string{"agents", "agents_dir", "result_max_bytes", "max_concurrent", "list_max_bytes", "wait_ms", "max_wait_ms", "keep_finished", "max_queued", "storage_dir", "statecontext"}
	// defaultNativeSubagentStateNames are the runs a model may still be waiting for.
	defaultNativeSubagentStateNames = []string{"queued", "running"}
	allowedNativeSubagentStates     = map[string]struct{}{
		"queued": {}, "running": {}, "done": {}, "failed": {}, "timed_out": {}, "stopped": {},
	}
	nativeSubagentAgentKeys = []string{"description", "model", "tools", "instructions", "prompt_documents", "max_tool_rounds", "max_run_tokens", "timeout_ms"}
	subagentNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// NativeSubagentSettings contains the normalized settings of native.subagent.
type NativeSubagentSettings struct {
	// ResultMaxBytes is the most child text one call returns; a longer answer is cut
	// and the result says so.
	ResultMaxBytes int64
	// MaxConcurrent is the most children that run at once. Children outlive a call, so
	// the limit counts every child of the same storage directory, across runs; the
	// session that first uses the directory sets it.
	MaxConcurrent int64
	// WaitMS is how long a call waits for the child's answer before it returns the
	// run's id and leaves the child running. Zero returns at once, so every call
	// detaches. The zero value of this struct therefore means "detach", not "default";
	// the loader always sets it.
	WaitMS int64
	// MaxWaitMS caps a wait the model asks for with wait_ms. The tool refuses a value
	// below one, because a zero cap would turn every wait into zero.
	MaxWaitMS int64
	// KeepFinished is how many finished runs stay readable by id; the oldest go first.
	// Runs still going are never dropped.
	KeepFinished int64
	// MaxQueued is how many runs may wait for a free slot while MaxConcurrent are
	// going. A start beyond it is refused. Zero allows no waiting at all.
	MaxQueued int64
	// ListMaxBytes is the most bytes of agent list the tool description may carry:
	// the full list when it fits, else the names, else a count. Zero never lists
	// inline, so the model has to ask. The zero value of this struct therefore means
	// "ask", not "default"; the loader always sets it.
	ListMaxBytes int64
	// StorageDir holds one transcript directory per child run.
	StorageDir string
	// StateContext bounds the run records copied into a compaction state-context
	// snapshot when statecontext.tools lists subagent.
	StateContext NativeSubagentStateContextSettings
	// AgentDirs are the absolute agents_dir directories in configured order.
	AgentDirs []string
	// Agents are the configured children: the inline ones in file order, then each
	// directory's folders in name order.
	Agents []NativeSubagentAgent
}

// NativeSubagentStateContextSettings selects which of the session's runs a
// state-context snapshot lists, and how many.
type NativeSubagentStateContextSettings struct {
	States []string
	Limit  int64
}

// NativeSubagentAgent is one configured child agent.
type NativeSubagentAgent struct {
	Name        string
	Description string
	// ModelPath is the absolute model file; empty means the calling session's model.
	ModelPath    string
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
	// Tools names the read-only native tools the child may use. HasTools is false
	// when the key is absent, which means every read-only native tool.
	Tools         []string
	HasTools      bool
	MaxToolRounds int64
	// MaxRunTokens is the most tokens the child may use; zero means no cap.
	MaxRunTokens int64
	TimeoutMS    int64
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
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
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
		if !containsString(nativeSubagentKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeSubagentKeys, "")
		}
	}
	settings := &NativeSubagentSettings{}
	if settings.ResultMaxBytes, err = configuredInteger(fields, "result_max_bytes", defaultNativeSubagentResultMaxBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxConcurrent, err = configuredInteger(fields, "max_concurrent", defaultNativeSubagentMaxConcurrent, 1, maxNativeSubagentConcurrent, settingPath, section); err != nil {
		return nil, err
	}
	if settings.ListMaxBytes, err = configuredInteger(fields, "list_max_bytes", defaultNativeSubagentListMaxBytes, 0, maxNativeSubagentListMaxBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.WaitMS, err = configuredInteger(fields, "wait_ms", defaultNativeSubagentWaitMS, 0, maxNativeSubagentWaitMS, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxWaitMS, err = configuredInteger(fields, "max_wait_ms", defaultNativeSubagentMaxWaitMS, 1, maxNativeSubagentWaitMS, settingPath, section); err != nil {
		return nil, err
	}
	if settings.KeepFinished, err = configuredInteger(fields, "keep_finished", defaultNativeSubagentKeepDone, 1, maxNativeSubagentKeepDone, settingPath, section); err != nil {
		return nil, err
	}
	if settings.MaxQueued, err = configuredInteger(fields, "max_queued", defaultNativeSubagentMaxQueued, 0, maxNativeSubagentMaxQueued, settingPath, section); err != nil {
		return nil, err
	}
	if settings.StorageDir, err = normalizeNativeSubagentStorageDir(fields, cwd, toolStorageDir, settingPath); err != nil {
		return nil, err
	}
	if settings.StateContext, err = normalizeNativeSubagentStateContext(fields["statecontext"], settingPath); err != nil {
		return nil, err
	}
	agentsRaw, hasAgents := fields["agents"]
	hasAgents = hasAgents && strings.TrimSpace(string(agentsRaw)) != "null"
	if settings.AgentDirs, err = normalizeSubagentAgentDirs(fields, cwd, settingPath); err != nil {
		return nil, err
	}
	noAgents := configError("%s: %s.agents must define at least one agent", settingPath, section)
	if !hasAgents && len(settings.AgentDirs) == 0 {
		return nil, noAgents
	}
	taken := make(map[string]string)
	if hasAgents {
		names, err := orderedObjectKeys(agentsRaw)
		if err != nil || (len(names) == 0 && len(settings.AgentDirs) == 0) {
			return nil, noAgents
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
	for _, dir := range settings.AgentDirs {
		agents, err := loadSubagentDirAgents(dir, settingPath, taken)
		if err != nil {
			return nil, err
		}
		settings.Agents = append(settings.Agents, agents...)
	}
	if len(settings.Agents) == 0 {
		return nil, configError("%s: %s.agents_dir holds no agent folder; add a folder with an agent.json or a prompt.md, or define agents", settingPath, section)
	}
	return settings, nil
}

func normalizeNativeSubagentStateContext(raw json.RawMessage, settingPath string) (NativeSubagentStateContextSettings, error) {
	settings := NativeSubagentStateContextSettings{States: append([]string(nil), defaultNativeSubagentStateNames...), Limit: defaultNativeSubagentStateLimit}
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	const section = "tools.native.subagent.statecontext"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return NativeSubagentStateContextSettings{}, configError("%s: %s must be an object", settingPath, section)
	}
	for key := range fields {
		if key != "states" && key != "limit" {
			return NativeSubagentStateContextSettings{}, unsupportedToolKey(settingPath+": "+section, key, []string{"states", "limit"}, "")
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
	return settings, nil
}

func normalizeNativeSubagentStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeSubagentStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.subagent.storage_dir must be a non-empty string", settingPath)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", configError("%s: tools.native.subagent.storage_dir could not be resolved: %v", settingPath, err)
	}
	return filepath.Clean(absolute), nil
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
	if agent.Tools, agent.HasTools, err = optionalSubagentTools(fields, settingPath, section); err != nil {
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
		contents, err := os.ReadFile(resolved)
		if err != nil {
			return nil, "", configError("%s: %s.prompt_documents[%d] could not be read: %s", settingPath, section, index, resolved)
		}
		parts = append(parts, string(contents))
		resolvedPaths = append(resolvedPaths, resolved)
	}
	return resolvedPaths, strings.Join(parts, "\n\n"), nil
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

func optionalSubagentTools(fields map[string]json.RawMessage, settingPath, section string) ([]string, bool, error) {
	raw, exists := fields["tools"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, false, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || names == nil {
		return nil, false, configError("%s: %s.tools must be an array of tool names", settingPath, section)
	}
	seen := make(map[string]struct{}, len(names))
	for index, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, false, configError("%s: %s.tools[%d] must be a non-empty string", settingPath, section, index)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, false, configError("%s: %s.tools lists %s twice", settingPath, section, name)
		}
		seen[name] = struct{}{}
		names[index] = name
	}
	return names, true, nil
}
