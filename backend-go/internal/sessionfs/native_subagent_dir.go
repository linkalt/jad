package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// subagentDirSettingsFile and subagentDirPromptFile are the two files of an agent
	// folder. Both are optional, but a folder needs at least one of them.
	subagentDirSettingsFile = "agent.json"
	subagentDirPromptFile   = "prompt.md"
	subagentDirSection      = "agent"
)

// normalizeSubagentAgentDirs reads tools.native.subagent.agents_dir: a list of
// directories, each holding one folder per agent. A relative path resolves from the
// session's cwd, like prompt_documents and the MCP config paths, so one project
// directory works from every session. A directory that does not exist is an error,
// not an empty list, so a typo cannot silently remove every agent. With missingOK
// (agents_dir_missing_ok) a directory that does not exist is skipped, so one toolset can
// list a general folder and the project's own folder, which a new project may not have;
// the session then simply has no agent from it. A path that exists but is not a
// directory is always an error. It returns the directories that exist and, separately,
// every configured one.
func normalizeSubagentAgentDirs(fields map[string]json.RawMessage, cwd, settingPath string, missingOK bool) (existing, configured []string, err error) {
	raw, exists := fields["agents_dir"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, nil, configError("%s: tools.native.subagent.agents_dir must be an array of directory paths", settingPath)
	}
	existing = make([]string, 0, len(values))
	configured = make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, nil, configError("%s: tools.native.subagent.agents_dir[%d] must be a non-empty string", settingPath, index)
		}
		directory := resolveFrom(cwd, expandHome(strings.TrimSpace(value)))
		if _, duplicate := seen[directory]; duplicate {
			return nil, nil, configError("%s: tools.native.subagent.agents_dir lists %s twice", settingPath, directory)
		}
		seen[directory] = struct{}{}
		configured = append(configured, directory)
		info, statErr := os.Stat(directory)
		if missingOK && errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !info.IsDir() {
			return nil, nil, configError("%s: tools.native.subagent.agents_dir[%d] is not a directory: %s", settingPath, index, directory)
		}
		existing = append(existing, directory)
	}
	return existing, configured, nil
}

// loadSubagentDirAgents loads every agent folder of one directory, in name order.
// taken maps an agent name to where it was first defined, so the same name in two
// places is refused instead of one silently winning. With override
// (agents_dir_override) a name already taken by an earlier folder is replaced by this
// one; a name taken inline (the folder is empty in taken) is still refused.
func loadSubagentDirAgents(directory, settingPath string, taken map[string]string, override bool) ([]NativeSubagentAgent, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, configError("%s: tools.native.subagent.agents_dir could not be read: %s: %v", settingPath, directory, err)
	}
	// os.ReadDir returns the entries sorted by filename, so the names come out in
	// name order without a sort of our own.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		// A symlink to a folder counts, so a shared agent can be linked in.
		info, err := os.Stat(filepath.Join(directory, entry.Name()))
		if err != nil || !info.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	agents := make([]NativeSubagentAgent, 0, len(names))
	for _, name := range names {
		folder := filepath.Join(directory, name)
		if !subagentNamePattern.MatchString(name) {
			return nil, configError("%s: tools.native.subagent.agents_dir: folder %s: the name must be 1 to 64 letters, digits, underscores or hyphens", settingPath, folder)
		}
		agent, err := loadSubagentFolder(name, folder)
		if err != nil {
			return nil, err
		}
		if previous, duplicate := taken[name]; duplicate && !(override && isSubagentFolderSource(previous)) {
			return nil, configError("%s: tools.native.subagent: agent %s is defined twice: %s and %s", settingPath, name, previous, folder)
		}
		taken[name] = folder
		agents = append(agents, agent)
	}
	return agents, nil
}

// loadSubagentFolder reads one agent folder: agent.json for the settings and
// prompt.md for the prompt. Relative paths inside agent.json (model, prompt_documents)
// resolve from the folder, so the folder can be copied or shared as a unit.
func loadSubagentFolder(name, folder string) (NativeSubagentAgent, error) {
	settingsPath := filepath.Join(folder, subagentDirSettingsFile)
	promptPath := filepath.Join(folder, subagentDirPromptFile)

	fields := map[string]json.RawMessage{}
	hasSettings, err := regularFileExists(settingsPath)
	if err != nil {
		return NativeSubagentAgent{}, configError("%s: could not read configuration: %v", settingsPath, err)
	}
	if hasSettings {
		raw, err := os.ReadFile(settingsPath)
		if err != nil {
			return NativeSubagentAgent{}, configError("%s: could not read configuration: %v", settingsPath, err)
		}
		if fields, err = readSessionObject(settingsPath, false); err != nil {
			return NativeSubagentAgent{}, err
		}
		if err := validateSchemaVersion(fields, settingsPath); err != nil {
			return NativeSubagentAgent{}, err
		}
		keys, err := orderedObjectKeys(raw)
		if err != nil {
			return NativeSubagentAgent{}, configError("%s: expected a JSON object", settingsPath)
		}
		for _, key := range keys {
			if key == "schema_version" {
				continue
			}
			if !containsString(nativeSubagentAgentKeys, key) {
				return NativeSubagentAgent{}, unsupportedToolKey(settingsPath+": "+subagentDirSection, key, nativeSubagentAgentKeys, "")
			}
		}
	}
	promptText := ""
	hasPrompt, err := regularFileExists(promptPath)
	if err != nil {
		return NativeSubagentAgent{}, configError("%s: could not read the prompt: %v", promptPath, err)
	}
	if hasPrompt {
		contents, err := os.ReadFile(promptPath)
		if err != nil {
			return NativeSubagentAgent{}, configError("%s: could not read the prompt: %v", promptPath, err)
		}
		promptText = strings.TrimSpace(string(contents))
	}
	if !hasSettings && !hasPrompt {
		return NativeSubagentAgent{}, configError("%s: agent folder %s has neither %s nor %s", folder, name, subagentDirSettingsFile, subagentDirPromptFile)
	}
	source := subagentSource{path: settingsPath, section: subagentDirSection, modelBase: folder, documentBase: folder}
	agent, err := buildNativeSubagentAgent(name, fields, source, promptText, promptPath)
	if err != nil {
		return NativeSubagentAgent{}, err
	}
	agent.Dir = folder
	return agent, nil
}

// regularFileExists is false for a missing file and an error for anything else that
// cannot be inspected, so a permission problem is not mistaken for "not there".
func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// isSubagentFolderSource tells a name taken by an agents_dir folder from one taken inline.
// loadSubagentDirAgents records a folder as its path; inline agents are recorded with a
// "(tools.native.subagent.agents)" suffix.
func isSubagentFolderSource(source string) bool {
	return !strings.HasSuffix(source, ")")
}

// withSubagentOverrides appends the agents of one folder to agents. A folder agent whose
// name an earlier folder already defined replaces that agent where it stood; the first
// inline entries (the first inline count of agents) are never replaced.
func withSubagentOverrides(agents, folder []NativeSubagentAgent, inline int) []NativeSubagentAgent {
	for _, agent := range folder {
		replaced := false
		for index := inline; index < len(agents); index++ {
			if agents[index].Name == agent.Name {
				agents[index] = agent
				replaced = true
				break
			}
		}
		if !replaced {
			agents = append(agents, agent)
		}
	}
	return agents
}
