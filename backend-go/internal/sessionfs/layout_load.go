package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"jad/backend-go/internal/catalog"
)

// sessionFileSuffix ends the name of a session file: <session id>.json in a session root.
const sessionFileSuffix = ".json"

// LoadLayoutSessionSettings is LoadSessionSettings for the catalog layout. It reads
// <id>.json, narrows the profile catalog by the session scope, takes the
// profile (requested, else the first in scope) and composes the setting-shaped map the
// existing normalizers read, so a normalizer needs no second copy.
//
// Every file is named "<category>/<name>", on the wire and in the files: the profile, the
// run's model, SessionConfig.Models and ModelDefault. ModelPath is the absolute file.
func LoadLayoutSessionSettings(location Location, layout Layout, requestedProfile, requestedModel string) (SessionConfig, error) {
	return loadLayoutSessionSettings(location, layout, requestedProfile, requestedModel, nil)
}

func loadLayoutSessionSettings(location Location, layout Layout, requestedProfile, requestedModel string, scans *CatalogScans) (SessionConfig, error) {
	sessionPath := location.File
	var session layoutSessionFile
	scans.noteFile(sessionPath)
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

	if scans == nil {
		scans = NewCatalogScans()
	}
	judged, err := judgeScope(layout, location, session, sessionPath, scans)
	if err != nil {
		return SessionConfig{}, err
	}
	// The default is the first profile whose file passes: a profile left out is warned
	// about, and a profile the caller names must pass itself.
	defaultProfile := session.defaultProfile(judged.usable)
	selected := requestedProfile
	if selected == "" {
		selected = defaultProfile
	}
	if selected == "" {
		if judged.all.Len() == 0 {
			// Files left out for their names are the likely reason, so they are named.
			return SessionConfig{}, configError("%s: scope.profiles matches no profile, so there is no profile to run%s", sessionPath, judged.skippedProfilesNote())
		}
		// No profile passes: the error is the one the first profile gets, which is what a load
		// that named no profile reported before profiles were left out.
		return SessionConfig{}, judged.verdicts[session.defaultProfile(judged.all)].err
	}
	entry, err := judged.all.Lookup(selected)
	if err != nil {
		return SessionConfig{}, configError("%s: %v", sessionPath, err)
	}
	if verdict := judged.verdicts[entry.ID()]; verdict.err != nil {
		return SessionConfig{}, verdict.err
	}
	keys, err := profileKeys(judged.all, sessionPath)
	if err != nil {
		return SessionConfig{}, err
	}
	profiles := judged.usable

	built := &layoutCompose{layout: layout, location: location, sessionPath: sessionPath, session: session, profiles: profiles, keys: keys, scans: scans, judged: judged, profileID: entry.ID()}
	built.warnings = judged.skippedNotes(sessionPath, session.defaultProfile(judged.all), defaultProfile)
	setting, modelPaths, err := built.compose(entry, nil)
	if err != nil {
		return SessionConfig{}, err
	}
	sources := built.settingSources(judged.verdicts[entry.ID()].file.Settings, setting)
	// A model that fails the model rules is not offered, so it cannot be loaded as the
	// requested one; the session loads without a model and a run reports the file's error.
	fault := judged.verdicts[entry.ID()].selectionFault(requestedModel)
	loadModelName := requestedModel
	if fault != nil && requestedModel != "" {
		loadModelName = ""
	}
	config, err := loadSessionSettingsFrom(location, setting, sessionPath, location.Root.pathBase(), keys[entry.ID()], loadModelName)
	if err != nil {
		return SessionConfig{}, err
	}
	if err := layout.checkPrivateDocuments(entry.ID(), entry.Path, config); err != nil {
		return SessionConfig{}, err
	}
	config.SettingSources = sources
	config.ProfileOwnDir, config.ProfileDataDir = layout.ProfileOwnDir(entry.ID()), layout.ProfileDataDir(entry.ID())
	if layout.Workspace != "" {
		config.WorkspaceLayout = true
		config.ProfileSelfEdit = effectiveSelfEdit(layout.ProfileSelfEdit, "")
		config.SelfEditPaths = layout.selfEditPaths()
	}
	if err := built.finishSandbox(&config); err != nil {
		return SessionConfig{}, err
	}
	if err := built.loadAgentToolsets(&config, entry); err != nil {
		return SessionConfig{}, err
	}
	if err := layout.checkWriteTargets(sessionPath, config); err != nil {
		return SessionConfig{}, err
	}
	config.guardChildWrites(layout.ProfileOwnRoots)
	if roots := layout.profilePrivateRoots(); len(roots) > 0 {
		// The in-process readers answer other profiles' private and data folders as missing,
		// with or without a sandbox; the profile's own stay readable (decider S3).
		shown := layout.profileShown(entry.ID())
		config.hideFixedPaths(roots, shown)
		for key, derived := range config.AgentToolsets {
			derived.hideFixedPaths(roots, shown)
			config.AgentToolsets[key] = derived
		}
	}
	config.RemotesDir = layout.Remotes
	// The normalizers know a profile only by its storage key; callers get the ID.
	config.Profile = entry.ID()
	config.ProfilePath = entry.Path
	config.DefaultProfile = defaultProfile
	config.Category = session.Category
	config.Profiles = profiles.IDs()
	config.ProfileNames = built.profileNames
	name := config.ModelDefault
	if requestedModel != "" {
		name = requestedModel
	}
	if name, _, err = SplitModelEffort(name); err != nil {
		return SessionConfig{}, err
	}
	config.ModelPath = modelPaths[name]
	config.ModelPaths = modelPaths
	own := judged.verdicts[entry.ID()].own
	if len(own.bases) > 0 {
		config.ModelBases = own.bases
	}
	for _, note := range own.notes {
		built.warn("%s: %s", sessionPath, note)
	}
	if fault != nil {
		config.ModelFault, config.ModelPath, config.ReasoningEffort = fault, "", ""
		if requestedModel == "" {
			// The profile's own default is the one that fails: the session has none, and none
			// of the others stands in for it.
			config.ModelDefault = ""
		}
	}
	if config.NativeSubagent != nil && config.NativeSubagent.ModelChoice {
		// A sub-agent picks among the profile's shared models only: the own ones are not
		// visible to it (decider 3c C2).
		shared := make(map[string]string, len(modelPaths))
		for id, path := range modelPaths {
			if !own.own(path) {
				shared[id] = path
			}
		}
		config.NativeSubagent.ProfileModels = shared
	}
	if config.ReasoningEffort != "" {
		// Refuse an effort the model does not offer now, before a run stores the user message.
		// Only the effort is checked: a model file that fails the model rules is reported by
		// the run that uses it, not by every load of the session.
		if err := checkModelEffortOnlyOf(config.ModelPath, config.ModelBases, config.WorkspaceLayout, config.ReasoningEffort); err != nil {
			return SessionConfig{}, err
		}
	}
	if err := resolveSubagentModels(config.NativeSubagent, layout, sessionPath, scans); err != nil {
		return SessionConfig{}, err
	}
	if err := resolveFallbackModels(config.ModelFallback, own, sessionPath, scans, built.warn); err != nil {
		return SessionConfig{}, err
	}
	config.Warnings = append(built.warnings, deprecatedAgentToolsWarnings(config.NativeSubagent, sessionPath)...)
	return config, nil
}

// resolveFallbackModels turns each model_fallback model ID into its model file and
// effort. Like an agent's model, a fallback may be any model of the catalog, not only one
// the profile lists; the catalog is the selected profile's, so its own models are found too.
// An unknown ID or an effort the model does not offer is a config error.
func resolveFallbackModels(settings *ModelFallbackSettings, own ownModels, sessionPath string, scans *CatalogScans, warn func(format string, args ...any)) error {
	if settings == nil {
		return nil
	}
	models := own.catalog
	kept := settings.Models[:0]
	for index := range settings.Models {
		entry := settings.Models[index]
		id, effort, err := SplitModelEffort(entry.ID)
		if err != nil {
			return configError("%s: model_fallback.models[%d]: %v", sessionPath, index, err)
		}
		found, err := models.Lookup(id)
		if err != nil {
			return configError("%s: model_fallback.models[%d]: %v", sessionPath, index, err)
		}
		// A fallback is optional: a model whose file fails is left out, as a run leaves out one
		// it cannot load (planRunStreamer), and the others still serve.
		if err := own.check(scans, found.Path); err != nil {
			warn("%s: model_fallback.models[%d]: model %s left out: %v", sessionPath, index, entry.ID, err)
			continue
		}
		if err := own.effort(found.Path, effort); err != nil {
			return configError("%s: model_fallback.models[%d]: %v", sessionPath, index, err)
		}
		entry.ModelPath, entry.ReasoningEffort = found.Path, effort
		kept = append(kept, entry)
	}
	settings.Models = kept
	return nil
}

// loadAgentToolsets reads the toolset each agent names. The session is composed again
// with that toolset in place of the profile's own, so its tools get the same normalizers,
// defaults and env_file as the profile's toolset, and the result is kept in
// config.AgentToolsets for the app to build the tools and connect the servers. The
// derived settings never carry a subagent tool, so an agent that names a toolset cannot list subagent.
func (c *layoutCompose) loadAgentToolsets(config *SessionConfig, selected catalog.Entry) error {
	if config.NativeSubagent == nil {
		return nil
	}
	var available *catalog.Catalog
	for _, agent := range config.NativeSubagent.Agents {
		key := agent.ToolsetKey()
		if key == "" {
			continue
		}
		if _, done := config.AgentToolsets[key]; done {
			continue
		}
		if available == nil {
			var err error
			if available, err = c.layout.ScanToolsets(c.scans); err != nil {
				return configError("%s: %v", c.sessionPath, err)
			}
		}
		for _, id := range agent.Toolset {
			if ownReference(id) {
				return configError("%s: tools.native.subagent agent %s: toolset %s: a profile's own toolset is used by that profile's own runs only, never by a sub-agent", c.sessionPath, agent.Name, id)
			}
			if _, err := available.Lookup(id); err != nil {
				return configError("%s: tools.native.subagent agent %s: toolset: %v", c.sessionPath, agent.Name, err)
			}
		}
		setting, _, err := c.compose(selected, agent.Toolset)
		if err == nil {
			var derived SessionConfig
			if derived, err = loadSessionSettingsFrom(c.location, setting, c.sessionPath, c.location.Root.pathBase(), c.keys[selected.ID()], ""); err == nil && derived.NativeProfileMessage != nil {
				err = configError("%s: tools.native.profile_message is not allowed in a toolset used by an agent (toolset %s); a sub-agent cannot message a profile", c.sessionPath, strings.Join(agent.Toolset, ", "))
			} else if err == nil {
				derived.NativeSubagent = nil
				if derived.Sandbox != nil && config.Sandbox != nil {
					derived.Sandbox.ReadOnlyPaths = config.Sandbox.ReadOnlyPaths
					c.addFixedHidePaths(derived.Sandbox)
				}
				c.setPlanSandboxReadOnlyPaths(derived, c.readOnly)
				if config.AgentToolsets == nil {
					config.AgentToolsets = make(map[string]SessionConfig)
				}
				config.AgentToolsets[key] = derived
				continue
			}
		}
		var configProblem *ConfigError
		if errors.As(err, &configProblem) {
			return configError("agent %s uses toolset %s: %s", agent.Name, key, configProblem.Message)
		}
		return err
	}
	return nil
}

// finishSandbox completes the session's sandbox once the settings are read: the folders of
// the session's own configuration become its read-only paths (sessionConfigPaths), and each
// agent's own sandbox is expanded and narrowed by the session's (resolveAgentSandboxes).
func (c *layoutCompose) finishSandbox(config *SessionConfig) error {
	readOnly := c.sessionConfigPaths(config)
	if config.Sandbox != nil {
		// The tools share this pointer (applySandbox), so they see the paths too. Its
		// isolation (also SessionConfig.Isolation) applies only to the runs a profile_message
		// starts in the profile: its own runs always work in its cwd (D7).
		config.Sandbox.ReadOnlyPaths = readOnly
		c.addFixedHidePaths(config.Sandbox)
		// Only the session's own sandbox, that of the profile's top-level runs, may write its
		// data folder; agent sandboxes, sandbox choices and the plan-mode fallback may not.
		config.Sandbox.ProfileData = config.ProfileDataDir
	}
	// The sandbox exec's read_only and plan-mode calls fall back to keeps them read-only too.
	c.setPlanSandboxReadOnlyPaths(*config, readOnly)
	c.readOnly = readOnly
	if err := c.finishSandboxChoices(config, readOnly); err != nil {
		return err
	}
	return c.resolveAgentSandboxes(config, readOnly)
}

// setPlanSandboxReadOnlyPaths gives the sandbox exec's read_only and plan-mode calls fall back
// to (NativeBashSettings.PlanSandbox) the session's read-only paths, and the layout's fixed
// hidden paths.
func (c *layoutCompose) setPlanSandboxReadOnlyPaths(config SessionConfig, readOnly []string) {
	if config.NativeExec != nil && config.NativeExec.Child.PlanSandbox != nil {
		config.NativeExec.Child.PlanSandbox.ReadOnlyPaths = append([]string(nil), readOnly...)
		c.addFixedHidePaths(config.NativeExec.Child.PlanSandbox)
	}
}

// addFixedHidePaths adds the layout's fixed paths (workspace layout only) to a sandbox after
// its hide_paths were read, so neither a profile's hide_paths nor a template can remove them:
// the hidden G/auth and other workspaces' .jad folders, and the sealed G and W/.jad with the
// helper folders of the tools the sandbox covers. The lists are copied, never appended to in
// place, because sandboxes may share them.
func (c *layoutCompose) addFixedHidePaths(sandbox *SandboxSettings) {
	if sandbox == nil || c.layout.Workspace == "" {
		return
	}
	sandbox.HidePaths = appendNewStrings(append([]string(nil), sandbox.HidePaths...), c.layout.FixedHidePaths...)
	sandbox.SealedPaths = appendNewStrings(append([]string(nil), sandbox.SealedPaths...), c.layout.SealedPaths...)
	inner := append([]string(nil), sandbox.SealedInner...)
	for _, tool := range slices.Sorted(maps.Keys(c.layout.SealedHelpers)) {
		if sandbox.Covers(tool) {
			inner = appendNewStrings(inner, c.layout.SealedHelpers[tool])
		}
	}
	sandbox.SealedInner = inner
	sandbox.ProfileHidden = appendNewStrings(append([]string(nil), sandbox.ProfileHidden...), c.layout.profilePrivateRoots()...)
	if c.profileID != "" {
		sandbox.ProfileShown = appendNewStrings(append([]string(nil), sandbox.ProfileShown...), c.layout.profileShown(c.profileID)...)
	}
}

// sessionConfigPaths are the files and folders the session's configuration comes from: the
// root setting, the session file, the catalogs (profiles, models, toolsets, MCP servers,
// remotes, sandbox templates) and the state folder with every session's transcripts. A
// sandbox keeps the ones under its writable paths read-only, so a sandboxed call cannot change
// what the next run is allowed, where jad sends its requests, or a transcript (D27, D28). The
// agent folders are not among them: an agent's sandbox is narrowed by the session's, so an
// edited agent.json cannot widen the environment, and agents may keep their own files there.
func (c *layoutCompose) sessionConfigPaths(config *SessionConfig) []string {
	candidates := []string{c.layout.RootSetting, c.location.File, c.layout.Profiles, c.layout.Models, c.layout.Tools, c.layout.MCP, c.layout.Remotes, c.layout.State, c.templates.dir,
		// Workspace layout only (empty otherwise): workspace.json, the private catalogs and
		// every session file of the workspace.
		c.layout.WorkspaceSetting, c.layout.WorkspaceModels, c.layout.WorkspaceTools, c.layout.SessionsDir}
	var paths []string
	for _, path := range candidates {
		if path != "" {
			paths = appendNewStrings(paths, filepath.Clean(path))
		}
	}
	return paths
}

// resolveAgentSandboxes expands each agent's sandbox value like a layer (a template name, or
// a template with overrides), reads it and narrows it by the session's: an agent that would
// reach more than the session is refused, naming the agent, the template and the path (D18).
// An agent whose sandbox is not enabled runs in its caller's. sandbox.isolation.mode is the
// agent key isolation; an agent may set only one of them.
func (c *layoutCompose) resolveAgentSandboxes(config *SessionConfig, readOnly []string) error {
	if config.NativeSubagent == nil {
		return nil
	}
	for index := range config.NativeSubagent.Agents {
		agent := &config.NativeSubagent.Agents[index]
		if agent.sandboxRaw == nil {
			continue
		}
		where := fmt.Sprintf("%s: tools.native.subagent agent %s", c.sessionPath, agent.Name)
		fields, reset, err := expandSandboxValue(agent.sandboxRaw, c.templates, where)
		if err != nil || reset || fields == nil {
			return err
		}
		if err := c.refuseV1ProfileData(mustJSON(fields), where); err != nil {
			return err
		}
		own, enabled, err := parseSandbox(mustJSON(fields), where, "sandbox")
		if err != nil {
			return err
		}
		if mode := own.Isolation.Mode; mode != "" {
			if agent.isolationKey {
				return configError("%s: isolation and sandbox.isolation.mode are both set; keep one", where)
			}
			if mode == SandboxIsolationWorktree && !agent.ChangesFiles() {
				return configError("%s: sandbox.isolation.mode worktree needs a tool that changes files (%s) in tools, or no tools list (an mcp: tool does not count: its server cannot be moved into a worktree)", where, strings.Join(agentChangeTools, ", "))
			}
			agent.Isolation = ""
			if mode == SandboxIsolationWorktree {
				agent.Isolation = SubagentIsolationWorktree
			}
		}
		if own.Isolation.Base == SandboxIsolationBaseWorking {
			agent.IsolationBase = SandboxIsolationBaseWorking
		}
		if own.Isolation.Materialize != "" {
			agent.IsolationMaterialize = own.Isolation.Materialize
		}
		if len(own.Isolation.SharePaths) > 0 {
			agent.IsolationShare = own.Isolation.SharePaths
		}
		if !enabled {
			continue
		}
		own.ReadOnlyPaths = readOnly
		c.addFixedHidePaths(own)
		narrowed, err := NarrowSandbox(config.Sandbox, own, nil, nil)
		if err != nil {
			template := ""
			if own.Template != "" {
				template = " (template " + own.Template + ")"
			}
			return configError("%s: sandbox%s %v", where, template, err)
		}
		agent.Sandbox = narrowed
	}
	return nil
}

// resolveSubagentModels turns an agent's model ID ("<category>/<name>") into the model
// file. A value ending in .json stays the file path the normalizer resolved. An agent may
// name any model of the catalog, not only the ones its caller's profile lists: a cheap
// reviewer model should not have to be listed in every main profile. An unknown ID is a
// config error that lists the available ones.
func resolveSubagentModels(settings *NativeSubagentSettings, layout Layout, sessionPath string, scans *CatalogScans) error {
	if settings == nil {
		return nil
	}
	var models *catalog.Catalog
	for index := range settings.Agents {
		agent := &settings.Agents[index]
		if agent.Model == "" {
			continue
		}
		if strings.HasSuffix(agent.Model, ".json") {
			continue
		}
		if strings.Contains(agent.Model, ".json"+effortSeparator) {
			return configError("%s: tools.native.subagent agent %s: model: a model file path cannot take %q<effort>; use a model ID", sessionPath, agent.Name, effortSeparator)
		}
		if models == nil {
			var err error
			if models, err = layout.ScanModels(scans); err != nil {
				return configError("%s: %v", sessionPath, err)
			}
		}
		modelID, effort, err := SplitModelEffort(agent.Model)
		if err != nil {
			return configError("%s: tools.native.subagent agent %s: model: %v", sessionPath, agent.Name, err)
		}
		if ownReference(modelID) {
			return configError("%s: tools.native.subagent agent %s: model %s: a profile's own model is used by that profile's own runs only, never by a sub-agent", sessionPath, agent.Name, modelID)
		}
		entry, err := models.Lookup(modelID)
		if err != nil {
			return configError("%s: tools.native.subagent agent %s: model: %v", sessionPath, agent.Name, err)
		}
		agent.ModelPath = entry.Path
		agent.ReasoningEffort = effort
	}
	return nil
}

// LoadLayoutSessionConfig loads the settings and the selected model file; a model that cannot be
// used is an error here (LoadSessionConfig is the one that lets such a session load).
func LoadLayoutSessionConfig(location Location, layout Layout, requestedProfile, requestedModel string) (SessionConfig, error) {
	config, err := LoadLayoutSessionSettings(location, layout, requestedProfile, requestedModel)
	if err != nil {
		return SessionConfig{}, err
	}
	if config.ModelPath == "" && config.ModelFault == nil {
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
	// profiles are the profiles of the scope that can be used; judged has the verdict on each
	// profile of the scope, left-out ones included.
	profiles *catalog.Catalog
	judged   scopeJudgement
	keys     map[string]string
	// profileNames maps a profile ID to the name its file sets; a profile without one is absent.
	profileNames map[string]string
	// scans is the catalog scans one list call shares between its sessions; nil scans
	// every catalog directory afresh.
	scans *CatalogScans
	// warnings are the notices of every compose of this load, without repeats.
	warnings []string
	// templates reads the session's sandbox templates; set by compose.
	templates sandboxTemplates
	// readOnly are the session's configuration paths (sessionConfigPaths); set by finishSandbox.
	readOnly []string
	// withoutToolsets composes the profile as if it named no toolset, to tell a failure of the
	// toolsets from one of the profile's own and the shared settings.
	withoutToolsets bool
	// sessionLayer is the session's settings with its top-level cwd; set by compose.
	sessionLayer map[string]json.RawMessage
	// profileID is the selected profile, whose private folders its sandboxes show.
	profileID string
}

func (c *layoutCompose) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if !slices.Contains(c.warnings, message) {
		c.warnings = append(c.warnings, message)
	}
}

// warnSkipped warns about each file of a scanned catalog that was left out for its name.
func (c *layoutCompose) warnSkipped(scanned *catalog.Catalog) {
	for _, note := range skippedFileNotes(c.sessionPath, scanned) {
		c.warn("%s", note)
	}
}

// compose builds the legacy-shaped setting for the selected profile. The returned map
// holds the file of each model ID the profile may use. Non-empty toolsets replace the
// profile's own toolsets, which is how the toolsets an agent names are read.
func (c *layoutCompose) compose(selected catalog.Entry, toolsets []string) (map[string]json.RawMessage, map[string]string, error) {
	c.scans.noteFile(selected.Path)
	verdict := c.judged.verdicts[selected.ID()]
	profile := verdict.file
	models, notes := verdict.offer(c.sessionPath, selected.ID())
	for _, note := range notes {
		c.warn("%s", note)
	}
	var err error
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
	c.sessionLayer = sessionLayer
	setting := mergeSettings(mergeSettings(c.layout.Defaults, sessionLayer), profile.Settings)
	if raw, set := setting["cwd"]; !set || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// Falling back to the root folder would run tools inside .jad/.
		return nil, nil, configError("%s: cwd is required; set defaults.cwd in the root setting, cwd or settings.cwd in the session, or settings.cwd in the profile", c.sessionPath)
	}
	if err := c.composeSandbox(setting, sessionLayer, profile.Settings, selected.Path); err != nil {
		return nil, nil, err
	}
	// env_file is a default for the toolset's bash, process, exec and tool_smith, not a setting of its own.
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
	if len(toolsets) == 0 {
		if !c.withoutToolsets {
			toolsets = profile.Toolset
		}
	} else {
		// Only the tools of this toolset are used. The state-context providers read the
		// session's own tools, which this toolset may not have.
		delete(setting, "statecontext")
	}
	if len(toolsets) > 0 {
		if err := c.addToolsets(setting, toolsets, selected.Path, defaultEnvFile); err != nil {
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
			if err := c.addProfileFields(entry, other.ID(), selected.Path, profile, models); err != nil {
				return nil, nil, err
			}
			c.noteProfileName(other.ID(), profile)
		} else {
			c.noteProfileName(other.ID(), c.judged.verdicts[other.ID()].file)
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

// composeSandbox replaces the generic merge of the sandbox key: each layer's value is expanded
// first (a template name, or {"template": ...} with overrides), then the layers are merged
// with the add_ lists added up (composeSandboxLayers). Expanding after the generic merge would
// lose a template named on one layer and overridden on another.
func (c *layoutCompose) composeSandbox(setting, sessionLayer, profileSettings map[string]json.RawMessage, profilePath string) error {
	layers := []json.RawMessage{c.layout.Defaults["sandbox"], sessionLayer["sandbox"], profileSettings["sandbox"]}
	wheres := []string{"root setting: defaults", c.sessionPath + ": settings", profilePath + ": settings"}
	var cwd string
	if err := json.Unmarshal(setting["cwd"], &cwd); err == nil {
		cwd = filepath.Clean(expandHome(strings.TrimSpace(cwd)))
	}
	c.templates = sandboxTemplates{dir: c.layout.SandboxTemplateDir(cwd), scans: c.scans}
	if c.templates.dir != "" {
		// A template file under a name that cannot be used is in no sandbox value's reach; the
		// warning is how its owner learns why a template does not apply.
		for _, skipped := range c.scans.skippedTemplates(c.templates.dir) {
			c.warn("%s: sandbox template file %s left out: %s", c.sessionPath, skipped.Path, skipped.Reason)
		}
	}
	composed, absent, err := composeSandboxLayers(layers, wheres, c.templates)
	if err != nil {
		return err
	}
	if err := c.refuseV1ProfileData(composed, c.sessionPath+": sandbox"); err != nil {
		return err
	}
	if absent {
		delete(setting, "sandbox")
		return nil
	}
	setting["sandbox"] = composed
	return nil
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
// storage (bash spill, records, tasks, fetch, processes, sub-agent runs, write/edit
// backups) lives under the session cwd, in toolBase, so the file tools reach it with no
// extra path rule and a project keeps its own tool state.
func (c *layoutCompose) storageEntry(profile catalog.Entry, toolBase string) map[string]any {
	dir := profileStateDir(c.layout, c.location.SessionID, profile.Category, profile.Name)
	return map[string]any{
		"messages":     filepath.Join(dir, "messages.jsonl"),
		"staging":      filepath.Join(dir, "staging"),
		"http_debug":   filepath.Join(dir, "debug", "http"),
		"tool_storage": filepath.Join(toolBase, c.location.SessionID, profile.Category, profile.Name, "tools"),
	}
}

func profileStateDir(layout Layout, sessionID, category, name string) string {
	return filepath.Join(layout.State, sessionID, category, name)
}

// ProfileMessagesPath is the transcript of a profile ID ("<category>/<name>") in this
// session, as LoadSessionSettings would report it, without loading anything. It is
// false for a location outside the catalog layout or an ID that is not two parts.
func (l Location) ProfileMessagesPath(profileID string) (string, bool) {
	if !l.UsesLayout() {
		return "", false
	}
	category, name, ok := strings.Cut(profileID, "/")
	if !ok || category == "" || name == "" {
		return "", false
	}
	return filepath.Join(profileStateDir(*l.Root.Layout, l.SessionID, category, name), "messages.jsonl"), true
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
func (c *layoutCompose) addProfileFields(entry map[string]any, profileID, profilePath string, profile layoutProfileFile, models *catalog.Catalog) error {
	if ids := models.IDs(); len(ids) > 0 {
		entry["models"] = ids
		entry["model_default"] = ids[0]
	} else if listed := c.judged.verdicts[profileID].listed; listed != nil && listed.Len() > 0 {
		// Every model it lists fails: it offers none, which is not the same as listing none.
		entry["models"] = []string{}
	}
	if id, _, _ := SplitModelEffort(profile.ModelDefault); profile.ModelDefault != "" {
		if _, offered := models.Get(id); offered {
			entry["model_default"] = profile.ModelDefault
		}
	}
	if profile.Instructions != nil {
		entry["instructions"] = profile.Instructions
	}
	if len(profile.PromptDocuments) > 0 {
		documents, err := c.layout.expandProfileOwn(profile.PromptDocuments, profileID, profilePath)
		if err != nil {
			return err
		}
		entry["prompt_documents"] = documents
	}
	if len(profile.Tools) > 0 {
		entry["tools"] = profile.Tools
	}
	return nil
}

// loadedToolset is one toolset file read for a compose: its ID, file and content.
type loadedToolset struct {
	id      string
	path    string
	toolset layoutToolsetFile
}

// addToolsets puts the native settings, MCP servers and hooks of the toolsets into the
// setting. Each toolset is "<category>/<name>" under tools/; the first listed wins a clash.
func (c *layoutCompose) addToolsets(setting map[string]json.RawMessage, ids []string, profilePath string, defaultEnvFile json.RawMessage) error {
	toolsets, err := c.layout.ScanToolsets(c.scans)
	if err != nil {
		return configError("%s: %v", profilePath, err)
	}
	c.warnSkipped(toolsets)
	loaded := make([]loadedToolset, 0, len(ids))
	for _, id := range ids {
		entry, err := toolsets.Lookup(id)
		if err != nil {
			return configError("%s: toolset: %v", profilePath, err)
		}
		var toolset layoutToolsetFile
		c.scans.noteFile(entry.Path)
		if err := readLayoutFile(entry.Path, &toolset); err != nil {
			return err
		}
		loaded = append(loaded, loadedToolset{id: id, path: entry.Path, toolset: toolset})
	}
	return c.applyToolsets(setting, loaded, defaultEnvFile)
}

// applyToolset puts one decoded toolset into the setting; path names its file in errors.
func (c *layoutCompose) applyToolset(setting map[string]json.RawMessage, toolset layoutToolsetFile, path string, defaultEnvFile json.RawMessage) error {
	return c.applyToolsets(setting, []loadedToolset{{id: path, path: path, toolset: toolset}}, defaultEnvFile)
}

// applyToolsets merges the toolsets in order. A native tool, an MCP server (by id) or the
// hooks object that an earlier toolset already gave is kept and the later one is
// ignored with a warning. Hooks in the settings and in a toolset stay an error.
func (c *layoutCompose) applyToolsets(setting map[string]json.RawMessage, loaded []loadedToolset, defaultEnvFile json.RawMessage) error {
	_, hooksInSettings := setting["hooks"]
	native := map[string]json.RawMessage{}
	nativeOwner := map[string]string{}
	extension := map[string]json.RawMessage{}
	extensionOwner := map[string]string{}
	var servers []map[string]any
	serverOwner := map[string]string{}
	var hooks json.RawMessage
	hooksOwner := ""
	for _, item := range loaded {
		if c.layout.globalToolset(item.path) {
			// Its relative paths resolve from G, not from the workspace base the session reads with.
			item.toolset = absoluteGlobalToolsetPaths(item.toolset, c.layout.GlobalDir)
		}
		if len(item.toolset.Native) > 0 {
			withEnv, err := withDefaultEnvFile(item.toolset.Native, defaultEnvFile, item.path)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(withEnv))
			for name := range withEnv {
				names = append(names, name)
			}
			slices.Sort(names)
			for _, name := range names {
				value := withEnv[name]
				if owner, taken := nativeOwner[name]; taken {
					c.warn("native tool %s is in toolsets %s and %s; using the one from %s", name, owner, item.id, owner)
					continue
				}
				native[name] = value
				nativeOwner[name] = item.id
			}
		}
		for _, name := range slices.Sorted(maps.Keys(item.toolset.Extension)) {
			if owner, taken := extensionOwner[name]; taken {
				c.warn("extension tool %s is in toolsets %s and %s; using the one from %s", name, owner, item.id, owner)
				continue
			}
			extension[name] = item.toolset.Extension[name]
			extensionOwner[name] = item.id
		}
		if len(item.toolset.Hooks) > 0 {
			if hooksInSettings {
				return configError("%s: hooks is set here and in the settings (defaults, session or profile); keep it in one place", item.path)
			}
			if hooksOwner != "" {
				c.warn("hooks are in toolsets %s and %s; using the hooks of %s and ignoring all hooks of %s", hooksOwner, item.id, hooksOwner, item.id)
			} else {
				hooks = item.toolset.Hooks
				hooksOwner = item.id
			}
		}
		own, err := c.mcpServers(item.toolset, item.path)
		if err != nil {
			return err
		}
		for _, server := range own {
			serverID, _ := server["id"].(string)
			if owner, taken := serverOwner[serverID]; taken {
				c.warn("MCP server %s is in toolsets %s and %s; using the one from %s", serverID, owner, item.id, owner)
				continue
			}
			serverOwner[serverID] = item.id
			servers = append(servers, server)
		}
	}
	if len(native) > 0 || len(extension) > 0 {
		tools := map[string]any{}
		if len(native) > 0 {
			tools["native"] = native
		}
		if len(extension) > 0 {
			tools["extension"] = extension
		}
		setting["tools"], _ = json.Marshal(tools)
	}
	if hooks != nil {
		setting["hooks"] = hooks
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
	available, err := c.scans.Scan("mcp", c.layout.MCP)
	if err != nil {
		return nil, configError("%s: %v", toolsetPath, err)
	}
	c.warnSkipped(available)
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
		item := map[string]any{"id": file.Name, "config_path": file.Path, "enabled": true, "token_path": MCPTokenPath(c.layout, key)}
		if use.Enabled != nil {
			item["enabled"] = *use.Enabled
		}
		if use.DenyTools != nil {
			item["deny_tools"] = use.DenyTools
		}
		if use.Resources != nil {
			item["resources"] = *use.Resources
		}
		if use.Prompts != nil {
			item["prompts"] = *use.Prompts
		}
		if use.Approval != nil {
			return nil, configError("%s: mcp %q: approval was removed; %s", toolsetPath, key, removedApprovalHint)
		}
		servers = append(servers, item)
	}
	return servers, nil
}

// envFileTools are the native tools that read an env_file.
var envFileTools = []string{"exec", "tool_smith"}

// withDefaultEnvFile gives each of exec and tool_smith that the toolset has, and that
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

// deprecatedAgentToolsWarnings names the agents whose tools list names only the opt-in tools
// of the old restricted child set: the list restricts nothing now (a child has every tool), so it
// is ignored and should be removed.
func deprecatedAgentToolsWarnings(settings *NativeSubagentSettings, sessionPath string) []string {
	if settings == nil {
		return nil
	}
	var warnings []string
	for _, agent := range settings.Agents {
		if agent.ToolsListDeprecated() {
			warnings = append(warnings, fmt.Sprintf("%s: tools.native.subagent agent %s: tools lists only %s, which a sub-agent has by default now; the list is ignored (remove it, or list the tools to allow)", sessionPath, agent.Name, strings.Join(agent.Tools, ", ")))
		}
	}
	return warnings
}
