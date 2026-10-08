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
	maxNativeSubagentConcurrent         int64 = 32
	defaultNativeSubagentStorageDir           = ".subagents"
	defaultNativeSubagentMaxToolRounds  int64 = 30
	defaultNativeSubagentTimeoutMS      int64 = 600_000
	maxNativeSubagentTimeoutMS          int64 = 3_600_000
	// maxNativeSubagentDescriptionChars bounds the text each agent adds to the tool
	// description, which is sent to the model on every request.
	maxNativeSubagentDescriptionChars = 500
)

var (
	nativeSubagentKeys      = []string{"agents", "result_max_bytes", "max_concurrent", "storage_dir"}
	nativeSubagentAgentKeys = []string{"description", "model", "tools", "instructions", "prompt_documents", "max_tool_rounds", "max_run_tokens", "timeout_ms"}
	subagentNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// NativeSubagentSettings contains the normalized settings of native.subagent.
type NativeSubagentSettings struct {
	// ResultMaxBytes is the most child text one call returns; a longer answer is cut
	// and the result says so.
	ResultMaxBytes int64
	// MaxConcurrent is the most children that run at once within one run.
	MaxConcurrent int64
	// StorageDir holds one transcript directory per child run.
	StorageDir string
	// Agents are the configured children in file order.
	Agents []NativeSubagentAgent
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
	// PromptText is the child's whole system prompt: Instructions first, then each
	// document in order, joined by a blank line. Empty means the child gets none.
	PromptText string
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
	if settings.StorageDir, err = normalizeNativeSubagentStorageDir(fields, cwd, toolStorageDir, settingPath); err != nil {
		return nil, err
	}
	agentsRaw, exists := fields["agents"]
	if !exists || strings.TrimSpace(string(agentsRaw)) == "null" {
		return nil, configError("%s: %s.agents must define at least one agent", settingPath, section)
	}
	names, err := orderedObjectKeys(agentsRaw)
	if err != nil || len(names) == 0 {
		return nil, configError("%s: %s.agents must define at least one agent", settingPath, section)
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
		settings.Agents = append(settings.Agents, agent)
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
	if agent.PromptDocuments, agent.PromptText, err = loadSubagentPrompt(fields, agent.Instructions, cwd, settingPath, section); err != nil {
		return NativeSubagentAgent{}, err
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
// not in the middle of a child run. The prompt is the instructions text first, then
// each document, joined by a blank line.
func loadSubagentPrompt(fields map[string]json.RawMessage, instructions, cwd, settingPath, section string) ([]string, string, error) {
	var paths []string
	if raw, exists := fields["prompt_documents"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &paths); err != nil || paths == nil {
			return nil, "", configError("%s: %s.prompt_documents must be an array of file paths", settingPath, section)
		}
	}
	parts := make([]string, 0, len(paths)+1)
	if instructions != "" {
		parts = append(parts, instructions)
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
