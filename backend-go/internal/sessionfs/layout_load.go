package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
)

const layoutSessionFileName = "session.json"

// LoadLayoutSessionSettings is LoadSessionSettings for the catalog layout. It reads
// <session>/session.json, narrows the profile catalog by the session scope, takes the
// profile (requested, else active_profile) and composes the setting-shaped map the
// existing normalizers read, so a normalizer or applyApprovalDefault needs no second copy.
//
// Every file is named "<category>/<name>", on the wire and in the files: the profile, the
// run's model, SessionConfig.Models and ModelDefault. ModelPath is the absolute file.
func LoadLayoutSessionSettings(location Location, layout Layout, requestedProfile, requestedModel string) (SessionConfig, error) {
	sessionPath := filepath.Join(location.Directory, layoutSessionFileName)
	var session layoutSessionFile
	if err := readLayoutFile(sessionPath, &session); err != nil {
		return SessionConfig{}, err
	}
	if session.SchemaVersion != 0 && session.SchemaVersion != 1 {
		return SessionConfig{}, configError("%s: unsupported schema_version %d", sessionPath, session.SchemaVersion)
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
	selected := requestedProfile
	if selected == "" {
		selected = session.ActiveProfile
	}
	if selected == "" {
		return SessionConfig{}, configError("%s: active_profile is required", sessionPath)
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
	setting, modelPaths, err := built.compose(entry)
	if err != nil {
		return SessionConfig{}, err
	}
	config, err := loadSessionSettingsFrom(location, setting, sessionPath, keys[entry.ID()], requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	// The normalizers know a profile only by its storage key; callers get the ID.
	config.Profile = entry.ID()
	config.DefaultProfile = session.ActiveProfile
	config.PathProfiles = profiles.IDs()
	name := requestedModel
	if name == "" {
		name = config.ModelDefault
	}
	config.ModelPath = modelPaths[name]
	return config, nil
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
}

// compose builds the legacy-shaped setting for the selected profile. The returned map
// holds the file of each model ID the profile may use.
func (c *layoutCompose) compose(selected catalog.Entry) (map[string]json.RawMessage, map[string]string, error) {
	var profile layoutProfileFile
	if err := readLayoutFile(selected.Path, &profile); err != nil {
		return nil, nil, err
	}
	if profile.SchemaVersion != 0 && profile.SchemaVersion != 1 {
		return nil, nil, configError("%s: unsupported schema_version %d", selected.Path, profile.SchemaVersion)
	}
	if err := checkLayoutSettings(profile.Settings, selected.Path+": settings"); err != nil {
		return nil, nil, err
	}

	models, err := catalog.Scan("model", c.layout.Models)
	if err != nil {
		return nil, nil, configError("%s: %v", selected.Path, err)
	}
	if models, err = models.Narrow(profile.Models); err != nil {
		return nil, nil, configError("%s: models: %v", selected.Path, err)
	}
	if profile.ModelDefault != "" {
		if _, err := models.Lookup(profile.ModelDefault); err != nil {
			return nil, nil, configError("%s: model_default: %v", selected.Path, err)
		}
	}
	modelPaths := make(map[string]string, models.Len())
	for _, item := range models.Entries() {
		modelPaths[item.ID()] = item.Path
	}

	setting := mergeSettings(mergeSettings(c.layout.Defaults, c.session.Settings), profile.Settings)
	setting["schema_version"] = json.RawMessage("1")
	if c.session.CWD != "" {
		if setting["cwd"], err = json.Marshal(c.session.CWD); err != nil {
			return nil, nil, err
		}
	}
	if profile.PromptRole != "" {
		setting["prompt_role"], _ = json.Marshal(profile.PromptRole)
	}
	if len(c.layout.ToolDefinitionPaths) > 0 {
		setting["tool_definition_paths"], _ = json.Marshal(c.layout.ToolDefinitionPaths)
	}
	if profile.Toolset != "" {
		if err := c.addToolset(setting, profile.Toolset, selected.Path); err != nil {
			return nil, nil, err
		}
	}

	entries := make(map[string]json.RawMessage, c.profiles.Len())
	for _, other := range c.profiles.Entries() {
		entry := c.storageEntry(other)
		if other.ID() == selected.ID() {
			c.addProfileFields(entry, other.ID(), profile, models)
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

// storageEntry is the part of a profile entry every profile gets: where its state lives,
// <state>/<session>/<category>/<name>/.
func (c *layoutCompose) storageEntry(profile catalog.Entry) map[string]any {
	dir := filepath.Join(c.layout.State, c.location.SessionID, profile.Category, profile.Name)
	return map[string]any{
		"messages":     filepath.Join(dir, "messages.jsonl"),
		"staging":      filepath.Join(dir, "staging"),
		"http_debug":   filepath.Join(dir, "debug", "http"),
		"tool_storage": filepath.Join(dir, "tools"),
	}
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
	if chosen := c.session.ModelDefaults[profileID]; chosen != "" {
		entry["model_default"] = chosen
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
func (c *layoutCompose) addToolset(setting map[string]json.RawMessage, id, profilePath string) error {
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
	if len(toolset.Native) > 0 {
		setting["tools"], _ = json.Marshal(map[string]any{"native": toolset.Native})
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
