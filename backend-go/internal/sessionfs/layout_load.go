package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
)

const layoutSessionFileName = "session.json"

// LoadLayoutSessionSettings is LoadSessionSettings for the catalog layout. It reads
// <session>/session.json, narrows the catalogs by the session scope, takes the profile
// (requested, else active_profile) and composes the setting-shaped map the existing
// normalizers read, so a normalizer or applyApprovalDefault needs no second copy.
//
// Model identity on the wire is the file name: requestedModel, SessionConfig.Models and
// ModelDefault hold names; ModelPath is the absolute file of the selected one.
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
	models, err := catalog.Scan("model", layout.Models)
	if err != nil {
		return SessionConfig{}, configError("%s: %v", sessionPath, err)
	}
	if profiles, err = profiles.Narrow(session.Scope.Profiles); err != nil {
		return SessionConfig{}, configError("%s: scope.profiles: %v", sessionPath, err)
	}
	if models, err = models.Narrow(session.Scope.Models); err != nil {
		return SessionConfig{}, configError("%s: scope.models: %v", sessionPath, err)
	}

	selected := requestedProfile
	if selected == "" {
		selected = session.ActiveProfile
	}
	if selected == "" {
		return SessionConfig{}, configError("%s: active_profile is required", sessionPath)
	}
	entry, err := uniqueEntry(profiles, selected, sessionPath, "profile")
	if err != nil {
		return SessionConfig{}, err
	}

	built := &layoutCompose{
		layout: layout, location: location, sessionPath: sessionPath, session: session,
		models: models, profiles: profiles,
	}
	setting, selectedModels, err := built.compose(entry)
	if err != nil {
		return SessionConfig{}, err
	}
	config, err := loadSessionSettingsFrom(location, setting, sessionPath, selected, requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	// loadSessionSettingsFrom resolved the model name as if it were a path; the name is the
	// requested model, else the profile's default, and the file is the catalog's.
	name := requestedModel
	if name == "" {
		name = config.ModelDefault
	}
	config.ModelPath = selectedModels[name]
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

// uniqueEntry returns the one entry called name, or an error that says what is
// available (missing) or which files clash (ambiguous).
func uniqueEntry(from *catalog.Catalog, name, path, kind string) (catalog.Entry, error) {
	found := from.Find(name)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return catalog.Entry{}, configError("%s: %s %q is not in the session scope; available: %s", path, kind, name, strings.Join(from.Names(), ", "))
	}
	paths := make([]string, len(found))
	for i, item := range found {
		paths[i] = item.Path
	}
	return catalog.Entry{}, configError("%s: %s %q exists %d times (%s); name it as <category>/<name>", path, kind, name, len(found), strings.Join(paths, ", "))
}

// layoutCompose holds what composing one session needs.
type layoutCompose struct {
	layout      Layout
	location    Location
	sessionPath string
	session     layoutSessionFile
	models      *catalog.Catalog
	profiles    *catalog.Catalog
}

// compose builds the legacy-shaped setting for the selected profile. The returned map
// of model names to files covers the models of that profile.
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

	profileModels, err := c.models.Narrow(profile.Models)
	if err != nil {
		return nil, nil, configError("%s: models: %v", selected.Path, err)
	}
	if err := profileModels.Unique(); err != nil {
		return nil, nil, configError("%s: %v", selected.Path, err)
	}
	modelPaths := make(map[string]string, profileModels.Len())
	for _, item := range profileModels.Entries() {
		modelPaths[item.Name] = item.Path
	}

	setting := mergeSettings(mergeSettings(c.layout.Defaults, c.session.Settings), profile.Settings)
	setting["schema_version"] = json.RawMessage("1")
	cwd := c.session.CWD
	if cwd != "" {
		if setting["cwd"], err = json.Marshal(cwd); err != nil {
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
	for _, item := range c.profiles.Names() {
		entry := c.storageEntry(item)
		if item == selected.Name {
			entry = c.selectedEntry(entry, profile, profileModels)
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, nil, err
		}
		entries[item] = raw
	}
	paths := map[string]any{"profiles": entries, "active_profile": selected.Name}
	if setting["paths"], err = json.Marshal(paths); err != nil {
		return nil, nil, err
	}
	return setting, modelPaths, nil
}

// storageEntry is the part of a profile entry every profile gets: where its state lives,
// <state>/<session>/<profile>/.
func (c *layoutCompose) storageEntry(profile string) map[string]any {
	dir := filepath.Join(c.layout.State, c.location.SessionID, profile)
	return map[string]any{
		"messages":     filepath.Join(dir, "messages.jsonl"),
		"staging":      filepath.Join(dir, "staging"),
		"http_debug":   filepath.Join(dir, "debug", "http"),
		"tool_storage": filepath.Join(dir, "tools"),
	}
}

// selectedEntry adds the fields only the selected profile needs.
func (c *layoutCompose) selectedEntry(entry map[string]any, profile layoutProfileFile, models *catalog.Catalog) map[string]any {
	names := models.Names()
	if len(names) > 0 {
		entry["models"] = names
		entry["model_default"] = names[0]
	}
	if profile.ModelDefault != "" {
		entry["model_default"] = profile.ModelDefault
	}
	if len(profile.PromptDocuments) > 0 {
		entry["prompt_documents"] = profile.PromptDocuments
	}
	if len(profile.Tools) > 0 {
		entry["tools"] = profile.Tools
	}
	return entry
}

// addToolset puts the toolset's native settings and MCP servers into the setting. A server
// key is a file name in the session's MCP scope, or <category>/<name> when the name
// exists in several categories; the file holds the connection, the toolset holds how
// this profile uses it.
func (c *layoutCompose) addToolset(setting map[string]json.RawMessage, name, profilePath string) error {
	toolsets, err := catalog.Scan("toolset", c.layout.Tools)
	if err != nil {
		return configError("%s: %v", profilePath, err)
	}
	entry, err := uniqueEntry(toolsets, name, profilePath, "toolset")
	if err != nil {
		return err
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
	if available, err = available.Narrow(c.session.Scope.MCP); err != nil {
		return nil, configError("%s: scope.mcp: %v", c.sessionPath, err)
	}
	servers := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		use := uses[key]
		selection, err := catalog.Of(key)
		if err != nil {
			return nil, configError("%s: mcp %v", toolsetPath, err)
		}
		found, err := available.Narrow(selection)
		if err != nil {
			return nil, configError("%s: mcp %v (the session scope.mcp decides which files are available)", toolsetPath, err)
		}
		if found.Len() != 1 {
			if err := found.Unique(); err != nil {
				return nil, configError("%s: mcp %v", toolsetPath, err)
			}
			return nil, configError("%s: mcp %q must name exactly one file, it matches %d", toolsetPath, key, found.Len())
		}
		file := found.Entries()[0]
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
