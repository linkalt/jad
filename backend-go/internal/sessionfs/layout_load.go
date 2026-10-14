package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/catalog"
)

// sessionFileSuffix ends the name of a session file: <session id>.json in a session root.
const sessionFileSuffix = ".json"

// LoadLayoutSessionSettings is LoadSessionSettings for the catalog layout. It reads
// <id>.json, narrows the profile catalog by the session scope, takes the
// profile (requested, else active_profile, else the first in scope) and composes the setting-shaped map the
// existing normalizers read, so a normalizer or applyApprovalDefault needs no second copy.
//
// Every file is named "<category>/<name>", on the wire and in the files: the profile, the
// run's model, SessionConfig.Models and ModelDefault. ModelPath is the absolute file.
func LoadLayoutSessionSettings(location Location, layout Layout, requestedProfile, requestedModel string) (SessionConfig, error) {
	sessionPath := location.File
	var session layoutSessionFile
	if err := readLayoutFile(sessionPath, &session); err != nil {
		return SessionConfig{}, err
	}
	if session.SchemaVersion != 0 && session.SchemaVersion != 1 {
		return SessionConfig{}, configError("%s: unsupported schema_version %d", sessionPath, session.SchemaVersion)
	}
	if err := session.refuseModelDefaults(sessionPath); err != nil {
		return SessionConfig{}, err
	}
	if err := checkLayoutSettings(session.Settings, sessionPath+": settings"); err != nil {
		return SessionConfig{}, err
	}
	if err := checkLayoutSettings(layout.Defaults, "root setting: defaults"); err != nil {
		return SessionConfig{}, err
	}

	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return SessionConfig{}, configError("%s: %v", sessionPath, err)
	}
	if profiles, err = profiles.Narrow(session.Scope.Profiles); err != nil {
		return SessionConfig{}, configError("%s: scope.profiles: %v", sessionPath, err)
	}
	defaultProfile := session.defaultProfile(profiles)
	selected := requestedProfile
	if selected == "" {
		selected = defaultProfile
	}
	if selected == "" {
		return SessionConfig{}, configError("%s: scope.profiles matches no profile, so there is no profile to run", sessionPath)
	}
	entry, err := profiles.Lookup(selected)
	if err != nil {
		return SessionConfig{}, configError("%s: %v", sessionPath, err)
	}
	keys, err := profileKeys(profiles, sessionPath)
	if err != nil {
		return SessionConfig{}, err
	}

	built := &layoutCompose{layout: layout, location: location, sessionPath: sessionPath, session: session, profiles: profiles, keys: keys}
	setting, modelPaths, err := built.compose(entry, "")
	if err != nil {
		return SessionConfig{}, err
	}
	config, err := loadSessionSettingsFrom(location, setting, sessionPath, location.Root.GlobalJadRoot, keys[entry.ID()], requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	if err := built.loadAgentToolsets(&config, entry); err != nil {
		return SessionConfig{}, err
	}
	// The normalizers know a profile only by its storage key; callers get the ID.
	config.Profile = entry.ID()
	config.DefaultProfile = defaultProfile
	config.Category = session.Category
	config.Profiles = profiles.IDs()
	config.ProfileNames = built.profileNames
	name := requestedModel
	if name == "" {
		name = config.ModelDefault
	}
	config.ModelPath = modelPaths[name]
	if err := resolveSubagentModels(config.NativeSubagent, layout.Models, sessionPath); err != nil {
		return SessionConfig{}, err
	}
	return config, nil
}

// loadAgentToolsets reads the toolset each agent names. The session is composed again
// with that toolset in place of the profile's own, so its tools get the same normalizers,
// defaults and env_file as the profile's toolset, and the result is kept in
// config.AgentToolsets for the app to build the tools and connect the servers. The
// derived settings never carry a subagent tool: a child cannot start a child.
func (c *layoutCompose) loadAgentToolsets(config *SessionConfig, selected catalog.Entry) error {
	if config.NativeSubagent == nil {
		return nil
	}
	var available *catalog.Catalog
	for _, agent := range config.NativeSubagent.Agents {
		if agent.Toolset == "" {
			continue
		}
		if _, done := config.AgentToolsets[agent.Toolset]; done {
			continue
		}
		if available == nil {
			var err error
			if available, err = catalog.Scan("toolset", c.layout.Tools); err != nil {
				return configError("%s: %v", c.sessionPath, err)
			}
		}
		if _, err := available.Lookup(agent.Toolset); err != nil {
			return configError("%s: tools.native.subagent agent %s: toolset: %v", c.sessionPath, agent.Name, err)
		}
		setting, _, err := c.compose(selected, agent.Toolset)
		if err == nil {
			var derived SessionConfig
			if derived, err = loadSessionSettingsFrom(c.location, setting, c.sessionPath, c.location.Root.GlobalJadRoot, c.keys[selected.ID()], ""); err == nil {
				derived.NativeSubagent = nil
				if config.AgentToolsets == nil {
					config.AgentToolsets = make(map[string]SessionConfig)
				}
				config.AgentToolsets[agent.Toolset] = derived
				continue
			}
		}
		var configProblem *ConfigError
		if errors.As(err, &configProblem) {
			return configError("agent %s uses toolset %s: %s", agent.Name, agent.Toolset, configProblem.Message)
		}
		return err
	}
	return nil
}

// resolveSubagentModels turns an agent's model ID ("<category>/<name>") into the model
// file. A value ending in .json stays the file path the normalizer resolved. An agent may
// name any model of the catalog, not only the ones its caller's profile lists: a cheap
// reviewer model should not have to be listed in every main profile. An unknown ID is a
// config error that lists the available ones.
func resolveSubagentModels(settings *NativeSubagentSettings, modelsDir, sessionPath string) error {
	if settings == nil {
		return nil
	}
	var models *catalog.Catalog
	for index := range settings.Agents {
		agent := &settings.Agents[index]
		if agent.Model == "" || strings.HasSuffix(agent.Model, ".json") {
			continue
		}
		if models == nil {
			var err error
			if models, err = catalog.Scan("model", modelsDir); err != nil {
				return configError("%s: %v", sessionPath, err)
			}
		}
		entry, err := models.Lookup(agent.Model)
		if err != nil {
			return configError("%s: tools.native.subagent agent %s: model: %v", sessionPath, agent.Name, err)
		}
		agent.ModelPath = entry.Path
	}
	return nil
}

// LoadLayoutSessionConfig loads the settings and the selected model file.
func LoadLayoutSessionConfig(location Location, layout Layout, requestedProfile, requestedModel string) (SessionConfig, error) {
	config, err := LoadLayoutSessionSettings(location, layout, requestedProfile, requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	if config.ModelPath == "" {
		return SessionConfig{}, configError("%s: profile %s has no model; list one in its models", config.SettingPath, config.Profile)
	}
	return config.WithModel()
}

// profileKeys maps each profile ID to the key the normalizers use in paths.profiles,
// which must match profileNamePattern (letters, digits, '_' and '-'). The key never
// leaves this package's composed map; two IDs that would share one are refused.
func profileKeys(profiles *catalog.Catalog, sessionPath string) (map[string]string, error) {
	replacer := strings.NewReplacer("/", "__", ".", "_d_")
	keys := make(map[string]string, profiles.Len())
	owners := make(map[string]string, profiles.Len())
	for _, id := range profiles.IDs() {
		key := replacer.Replace(id)
		if other, taken := owners[key]; taken {
			return nil, configError("%s: profiles %q and %q cannot both be used: they map to the same internal key %q", sessionPath, other, id, key)
		}
		owners[key] = id
		keys[id] = key
	}
	return keys, nil
}

// layoutCompose holds what composing one session needs.
type layoutCompose struct {
	layout      Layout
	location    Location
	sessionPath string
	session     layoutSessionFile
	profiles    *catalog.Catalog
	keys        map[string]string
	// profileNames maps a profile ID to the name its file sets; a profile without one is absent.
	profileNames map[string]string
}

// compose builds the legacy-shaped setting for the selected profile. The returned map
// holds the file of each model ID the profile may use. A non-empty toolset replaces the
// profile's own toolset, which is how the toolset an agent names is read.
func (c *layoutCompose) compose(selected catalog.Entry, toolset string) (map[string]json.RawMessage, map[string]string, error) {
	allModels, err := catalog.Scan("model", c.layout.Models)
	if err != nil {
		return nil, nil, configError("%s: %v", selected.Path, err)
	}
	profile, models, err := readProfile(selected.Path, allModels)
	if err != nil {
		return nil, nil, err
	}
	modelPaths := make(map[string]string, models.Len())
	for _, item := range models.Entries() {
		modelPaths[item.ID()] = item.Path
	}

	// cwd: root defaults, then the session (its top-level cwd beats its settings.cwd), then
	// the profile.
	sessionLayer := mergeSettings(nil, c.session.Settings)
	if c.session.CWD != "" {
		if sessionLayer["cwd"], err = json.Marshal(c.session.CWD); err != nil {
			return nil, nil, err
		}
	}
	setting := mergeSettings(mergeSettings(c.layout.Defaults, sessionLayer), profile.Settings)
	if _, set := setting["cwd"]; !set {
		// Falling back to the root folder would run tools inside .jad/.
		return nil, nil, configError("%s: cwd is required; set defaults.cwd in the root setting, cwd or settings.cwd in the session, or settings.cwd in the profile", c.sessionPath)
	}
	// env_file is a default for the toolset's bash, process and exec, not a setting of its own.
	defaultEnvFile := setting["env_file"]
	setting["schema_version"] = json.RawMessage("1")
	if c.session.Name != "" {
		if setting["session_name"], err = json.Marshal(c.session.Name); err != nil {
			return nil, nil, err
		}
	}
	if profile.PromptRole != "" {
		setting["prompt_role"], _ = json.Marshal(profile.PromptRole)
	}
	if len(c.layout.ToolDefinitionPaths) > 0 {
		setting["tool_definition_paths"], _ = json.Marshal(c.layout.ToolDefinitionPaths)
	}
	if toolset == "" {
		toolset = profile.Toolset
	} else {
		// Only the tools of this toolset are used. The state-context providers read the
		// session's own tools, which this toolset may not have.
		delete(setting, "statecontext")
	}
	if toolset != "" {
		if err := c.addToolset(setting, toolset, selected.Path, defaultEnvFile); err != nil {
			return nil, nil, err
		}
	}

	entries := make(map[string]json.RawMessage, c.profiles.Len())
	toolBase, err := toolStorageBase(setting)
	if err != nil {
		return nil, nil, configError("%s: %v", c.sessionPath, err)
	}
	c.profileNames = map[string]string{}
	for _, other := range c.profiles.Entries() {
		entry := c.storageEntry(other, toolBase)
		if other.ID() == selected.ID() {
			c.addProfileFields(entry, other.ID(), profile, models)
			c.noteProfileName(other.ID(), profile)
		} else {
			unselected, _, err := readProfile(other.Path, allModels)
			if err == nil {
				err = checkUnselectedProfile(other.Path, unselected)
			}
			if err != nil {
				return nil, nil, err
			}
			c.noteProfileName(other.ID(), unselected)
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, nil, err
		}
		entries[c.keys[other.ID()]] = raw
	}
	paths := map[string]any{"profiles": entries, "active_profile": c.keys[selected.ID()]}
	if setting["paths"], err = json.Marshal(paths); err != nil {
		return nil, nil, err
	}
	return setting, modelPaths, nil
}

func (c *layoutCompose) noteProfileName(id string, profile layoutProfileFile) {
	if name := strings.TrimSpace(profile.Name); name != "" {
		c.profileNames[id] = name
	}
}

// storageEntry is the part of a profile entry every profile gets: where its state lives,
// <state>/<session>/<category>/<name>/.
//
// The transcript, staging and HTTP debug stay under the root state folder. The tool
// storage (bash spill, memory, tasks, fetch, processes, sub-agent runs, write/edit
// backups) lives under the session cwd, in toolBase, so the file tools reach it with no
// extra path rule and a project keeps its own tool state.
func (c *layoutCompose) storageEntry(profile catalog.Entry, toolBase string) map[string]any {
	dir := filepath.Join(c.layout.State, c.location.SessionID, profile.Category, profile.Name)
	return map[string]any{
		"messages":     filepath.Join(dir, "messages.jsonl"),
		"staging":      filepath.Join(dir, "staging"),
		"http_debug":   filepath.Join(dir, "debug", "http"),
		"tool_storage": filepath.Join(toolBase, c.location.SessionID, profile.Category, profile.Name, "tools"),
	}
}

// defaultToolStorageDir is the folder under the cwd that holds tool storage when
// tool_storage_dir is not set.
const defaultToolStorageDir = ".jad/state"

// toolStorageBase is the folder that holds the tool storage of every profile of the
// session: tool_storage_dir (default defaultToolStorageDir), relative to the cwd. A profile's
// tool storage is <base>/<session>/<category>/<name>/tools.
func toolStorageBase(setting map[string]json.RawMessage) (string, error) {
	cwd, dir := "", defaultToolStorageDir
	if err := json.Unmarshal(setting["cwd"], &cwd); err != nil {
		return "", fmt.Errorf("cwd must be a string")
	}
	if raw, ok := setting["tool_storage_dir"]; ok {
		if err := json.Unmarshal(raw, &dir); err != nil || strings.TrimSpace(dir) == "" {
			return "", fmt.Errorf("tool_storage_dir must be a non-empty string")
		}
	}
	return resolveFrom(expandHome(strings.TrimSpace(cwd)), expandHome(strings.TrimSpace(dir))), nil
}

// addProfileFields adds the fields only the selected profile needs.
func (c *layoutCompose) addProfileFields(entry map[string]any, profileID string, profile layoutProfileFile, models *catalog.Catalog) {
	if ids := models.IDs(); len(ids) > 0 {
		entry["models"] = ids
		entry["model_default"] = ids[0]
	}
	if profile.ModelDefault != "" {
		entry["model_default"] = profile.ModelDefault
	}
	if profile.Instructions != nil {
		entry["instructions"] = profile.Instructions
	}
	if len(profile.PromptDocuments) > 0 {
		entry["prompt_documents"] = profile.PromptDocuments
	}
	if len(profile.Tools) > 0 {
		entry["tools"] = profile.Tools
	}
}

// addToolset puts the toolset's native settings and MCP servers into the setting. The
// toolset is "<category>/<name>" under tools/.
func (c *layoutCompose) addToolset(setting map[string]json.RawMessage, id, profilePath string, defaultEnvFile json.RawMessage) error {
	toolsets, err := catalog.Scan("toolset", c.layout.Tools)
	if err != nil {
		return configError("%s: %v", profilePath, err)
	}
	entry, err := toolsets.Lookup(id)
	if err != nil {
		return configError("%s: toolset: %v", profilePath, err)
	}
	var toolset layoutToolsetFile
	if err := readLayoutFile(entry.Path, &toolset); err != nil {
		return err
	}
	return c.applyToolset(setting, toolset, entry.Path, defaultEnvFile)
}

// applyToolset puts one decoded toolset into the setting; path names its file in errors.
func (c *layoutCompose) applyToolset(setting map[string]json.RawMessage, toolset layoutToolsetFile, path string, defaultEnvFile json.RawMessage) error {
	entry := catalog.Entry{Path: path}
	if len(toolset.Native) > 0 {
		native, err := withDefaultEnvFile(toolset.Native, defaultEnvFile, entry.Path)
		if err != nil {
			return err
		}
		setting["tools"], _ = json.Marshal(map[string]any{"native": native})
	}
	if len(toolset.Hooks) > 0 {
		if _, set := setting["hooks"]; set {
			return configError("%s: hooks is set here and in the settings (defaults, session or profile); keep it in one place", entry.Path)
		}
		setting["hooks"] = toolset.Hooks
	}
	servers, err := c.mcpServers(toolset, entry.Path)
	if err != nil {
		return err
	}
	if len(servers) > 0 {
		setting["mcp_servers"], _ = json.Marshal(servers)
	}
	return nil
}

// mcpServers turns the toolset's mcp object into the server list. Each key is
// "<category>/<name>" of a file under mcp/; the file holds the connection and its
// environment, the toolset holds how this profile uses it. The server id (and so the
// model-facing tool name mcp:<id>:<tool>) is the file name, so two files with the same
// name cannot both be in one toolset.
func (c *layoutCompose) mcpServers(toolset layoutToolsetFile, toolsetPath string) ([]map[string]any, error) {
	if len(toolset.MCP) == 0 {
		return nil, nil
	}
	keys, err := orderedObjectKeys(toolset.MCP)
	if err != nil {
		return nil, configError("%s: mcp must be an object", toolsetPath)
	}
	var uses map[string]layoutToolsetMCP
	decoder := json.NewDecoder(strings.NewReader(string(toolset.MCP)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&uses); err != nil {
		return nil, configError("%s: mcp: %v", toolsetPath, err)
	}
	available, err := catalog.Scan("mcp", c.layout.MCP)
	if err != nil {
		return nil, configError("%s: %v", toolsetPath, err)
	}
	servers := make([]map[string]any, 0, len(keys))
	ids := make(map[string]string, len(keys))
	for _, key := range keys {
		file, err := available.Lookup(key)
		if err != nil {
			return nil, configError("%s: mcp: %v", toolsetPath, err)
		}
		if other, taken := ids[file.Name]; taken {
			return nil, configError("%s: mcp: %q and %q have the same file name, so the same tool names; use one of them", toolsetPath, other, key)
		}
		ids[file.Name] = key
		use := uses[key]
		item := map[string]any{"id": file.Name, "config_path": file.Path, "enabled": true}
		if use.Enabled != nil {
			item["enabled"] = *use.Enabled
		}
		if use.DenyTools != nil {
			item["deny_tools"] = use.DenyTools
		}
		if use.Approval != nil {
			item["approval"] = *use.Approval
		}
		servers = append(servers, item)
	}
	return servers, nil
}

// envFileTools are the native tools that read an env_file.
var envFileTools = []string{"bash", "process", "exec"}

// withDefaultEnvFile gives each of bash, process and exec that the toolset has, and that
// names no env_file, the default env_file. It adds no tool and does not change the toolset's
// own map.
func withDefaultEnvFile(native map[string]json.RawMessage, envFile json.RawMessage, toolsetPath string) (map[string]json.RawMessage, error) {
	if len(envFile) == 0 {
		return native, nil
	}
	result := make(map[string]json.RawMessage, len(native))
	for name, value := range native {
		result[name] = value
	}
	for _, name := range envFileTools {
		raw, present := native[name]
		if !present {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, configError("%s: native.%s must be an object", toolsetPath, name)
		}
		if _, named := fields["env_file"]; named {
			continue
		}
		fields["env_file"] = envFile
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		result[name] = encoded
	}
	return result, nil
}
