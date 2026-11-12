package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The workspace layout (v2, docs/design_workspace_layout.md). It is on only when the start
// directory has .jad/workspace.json and no root setting is named; otherwise Load is v1 and
// nothing here runs. The global folder G holds the shared catalogs; each workspace W keeps its
// sessions, profiles, private models and tools, and state in W/.jad. Both files are read once
// at start.

const (
	workspaceSettingName = "workspace.json"
	globalSettingName    = "global.json"
	// v1RootSettingName is the v1 setting a v2 start does not read in G or W.
	v1RootSettingName = "setting.json"
	// workspaceSchemaVersion is the only schema_version global.json and workspace.json take.
	workspaceSchemaVersion = 2
)

var (
	globalSettingKeys    = []string{"schema_version", "server", "cli", "workspaces", "catalogs", "defaults", "libraries"}
	workspaceSettingKeys = []string{"schema_version", "id", "label", "defaults", "libraries"}
	workspaceEntryKeys   = []string{"id", "label", "path"}
	// globalCatalogKeys are the catalogs global.json may move; profiles and state belong to
	// each workspace and have fixed folders there.
	globalCatalogKeys = []string{"models", "tools", "mcp", "remotes", "sandbox"}
	// workspaceIDCleaner turns a folder name into a root ID.
	workspaceIDCleaner = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// Workspace is one workspace of the workspace layout, for the API: a listed workspace that is
// missing or broken is kept with the reason it is unavailable, and has no root.
type Workspace struct {
	ID        string
	Label     string
	Directory string
	// Start is the workspace the server was started in.
	Start bool
	// Unavailable is why a listed workspace was skipped; empty when it is served.
	Unavailable string
}

// workspaceAt reports whether dir has .jad/workspace.json as a regular file. A directory of
// that name is an error, so a typo never falls back to v1 silently.
func workspaceAt(dir string) (bool, error) {
	path := filepath.Join(dir, ".jad", workspaceSettingName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s: must be a file", path)
	}
	return true, nil
}

// globalDir is G: LoadOptions.GlobalDir, JAD_GLOBAL_DIR, or $HOME/.jad.
func globalDir(cwd, configured string, env map[string]string) (string, error) {
	value := strings.TrimSpace(configured)
	if value == "" {
		value = strings.TrimSpace(env["JAD_GLOBAL_DIR"])
	}
	home := strings.TrimSpace(env["HOME"])
	if value == "" {
		if home == "" {
			return "", errors.New("workspace layout: the global folder is unknown; set HOME or JAD_GLOBAL_DIR")
		}
		return filepath.Join(home, ".jad"), nil
	}
	return resolveFrom(cwd, expandHomeWith(value, home)), nil
}

// expandHomeWith expands a leading ~ with home (the environment's HOME, so tests are
// deterministic); without a home the value is returned as it is.
func expandHomeWith(value, home string) string {
	if home == "" {
		return value
	}
	if value == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(value, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return value
}

// realPath is path with symlinks resolved, or the clean path when it does not exist.
func realPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// workspaceSpec is one workspace before its root is built.
type workspaceSpec struct {
	id, label, dir, real string
	setting              map[string]json.RawMessage
	start                bool
}

// loadWorkspaces is Load for the workspace layout: the start directory cwd has
// .jad/workspace.json.
func loadWorkspaces(cwd, configuredGlobal string, env map[string]string) (Config, error) {
	home := strings.TrimSpace(env["HOME"])
	global, err := globalDir(cwd, configuredGlobal, env)
	if err != nil {
		return Config{}, err
	}
	globalReal := realPath(global)
	var warnings []string
	globalPath := filepath.Join(global, globalSettingName)
	setting := map[string]json.RawMessage{}
	if info, statErr := os.Stat(globalPath); statErr == nil && info.Mode().IsRegular() {
		if setting, err = readObject(globalPath); err != nil {
			return Config{}, err
		}
	} else if statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: must be a readable file", globalPath)
	} else {
		globalPath = ""
	}
	where := globalPath
	if where == "" {
		where = filepath.Join(global, globalSettingName)
	}
	if err := checkWorkspaceFileKeys(setting, where, globalSettingKeys); err != nil {
		return Config{}, err
	}
	for _, dir := range []string{global, filepath.Join(cwd, ".jad")} {
		if info, statErr := os.Stat(filepath.Join(dir, v1RootSettingName)); statErr == nil && info.Mode().IsRegular() {
			warnings = append(warnings, fmt.Sprintf("%s is not read in the workspace layout (started in %s, which has .jad/%s)", filepath.Join(dir, v1RootSettingName), cwd, workspaceSettingName))
		}
	}

	base, err := globalLayout(setting, where, global)
	if err != nil {
		return Config{}, err
	}
	globalDefaults, err := workspaceDefaults(setting, where, global, global, home)
	base.RootSetting = globalPath
	if err != nil {
		return Config{}, err
	}

	start, err := startWorkspace(cwd)
	if err != nil {
		return Config{}, err
	}
	listed, err := listedWorkspaces(setting, where, home)
	if err != nil {
		return Config{}, err
	}
	specs, workspaces, skipped, err := pickWorkspaces(start, listed, globalReal, where)
	if err != nil {
		return Config{}, err
	}
	warnings = append(warnings, skipped...)

	// Every .jad folder of a known workspace, served or not, is hidden from the others, as
	// written and with links resolved (a sandbox binds real paths).
	jadFolders := make(map[string][]string, len(workspaces))
	profileOwnRoots := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		jad := filepath.Join(workspace.Directory, ".jad")
		jadFolders[workspace.ID] = appendNewStrings([]string{jad}, realPath(jad))
		profileOwnRoots = appendNewStrings(profileOwnRoots, filepath.Join(jad, profileOwnFolder))
	}
	auth := appendNewStrings([]string{filepath.Join(global, "auth")}, filepath.Join(globalReal, "auth"))
	roots := make([]Root, 0, len(specs))
	for _, spec := range specs {
		layout, err := spec.layout(base, globalDefaults, home)
		if err != nil {
			return Config{}, err
		}
		layout.FixedHidePaths = slices.Clone(auth)
		ownJad := filepath.Join(spec.dir, ".jad")
		layout.ProfileOwnRoots = appendNewStrings(slices.Clone(profileOwnRoots), filepath.Join(ownJad, profileOwnFolder))
		layout.SealedPaths = appendNewStrings(appendNewStrings([]string{global}, globalReal), appendNewStrings([]string{ownJad}, realPath(ownJad))...)
		// The default functions_dir of each tool when the session works in W (cwd W/.jad/<dir>).
		layout.SealedHelpers = map[string]string{"exec": filepath.Join(ownJad, "exec"), "tool_smith": filepath.Join(ownJad, "functions")}
		for _, workspace := range workspaces {
			if workspace.ID != spec.id {
				layout.FixedHidePaths = appendNewStrings(layout.FixedHidePaths, jadFolders[workspace.ID]...)
			}
		}
		roots = append(roots, Root{ID: spec.id, Label: spec.label, Directory: layout.SessionsDir, GlobalJadRoot: global, Layout: layout})
		if spec.start && !defaultsEnableSandbox(layout.Defaults) {
			warnings = append(warnings, fmt.Sprintf("workspace %s: the global and workspace defaults do not enable a sandbox, so tool calls run as you and can read and write every workspace; private means separate storage and names only (docs/design_workspace_layout.md §6)", spec.id))
		}
	}

	serverSettings, err := normalizeServerSettings(setting["server"], where+": server", global)
	if err != nil {
		return Config{}, err
	}
	if serverSettings.LogFile != "" {
		serverSettings.LogFile = resolveFrom(global, serverSettings.LogFile)
	}
	cliSettings, err := normalizeCLISettings(setting["cli"], where)
	if err != nil {
		return Config{}, err
	}
	return Config{Roots: roots, Server: serverSettings, CLI: cliSettings, SettingPath: globalPath, Warnings: warnings, Workspaces: workspaces, GlobalDir: global}, nil
}

// checkWorkspaceFileKeys refuses a key the file does not read and a schema_version other than 2.
func checkWorkspaceFileKeys(setting map[string]json.RawMessage, where string, allowed []string) error {
	for key := range setting {
		if !slices.Contains(allowed, key) {
			return configError("%s: %s is not a key of this file (known: %s)", where, key, strings.Join(allowed, ", "))
		}
	}
	if raw, exists := setting["schema_version"]; exists {
		if version, ok := configSafeInteger(raw); !ok || version != workspaceSchemaVersion {
			return configError("%s: unsupported schema_version %s; the workspace layout is schema_version %d", where, strings.TrimSpace(string(raw)), workspaceSchemaVersion)
		}
	}
	return nil
}

// globalLayout is the part of every workspace's layout that global.json gives: the shared
// catalogs, resolved from G, and libraries.tool_definition_paths.
func globalLayout(setting map[string]json.RawMessage, where, global string) (Layout, error) {
	folders := map[string]string{}
	for _, key := range globalCatalogKeys {
		folders[key] = key
	}
	if raw, exists := setting["catalogs"]; exists {
		var configured map[string]json.RawMessage
		if err := json.Unmarshal(raw, &configured); err != nil || configured == nil {
			return Layout{}, configError("%s: catalogs must be an object", where)
		}
		for key, rawValue := range configured {
			if !slices.Contains(globalCatalogKeys, key) {
				return Layout{}, configError("%s: catalogs.%s is not a global catalog (%s); profiles, sessions and state belong to each workspace", where, key, strings.Join(globalCatalogKeys, ", "))
			}
			var folder string
			if err := json.Unmarshal(rawValue, &folder); err != nil || strings.TrimSpace(folder) == "" {
				return Layout{}, configError("%s: catalogs.%s must be a non-empty string", where, key)
			}
			folders[key] = strings.TrimSpace(folder)
		}
	}
	layout := Layout{
		Models:    resolveFrom(global, folders["models"]),
		Tools:     resolveFrom(global, folders["tools"]),
		MCP:       resolveFrom(global, folders["mcp"]),
		Remotes:   resolveFrom(global, folders["remotes"]),
		GlobalDir: global,
	}
	if err := layout.setSandboxCatalog(folders["sandbox"], global, where); err != nil {
		return Layout{}, err
	}
	libraries, err := librariesObject(setting, where)
	if err != nil {
		return Layout{}, err
	}
	libraryFolders, err := parseLibraryFolders(libraries, where, global, "tool_definition_paths", "max_asset_bytes")
	if err != nil {
		return Layout{}, err
	}
	layout.Libraries = &Libraries{GlobalDir: global, Global: libraryFolders}
	if raw, exists := libraries["max_asset_bytes"]; exists {
		value, ok := configSafeInteger(raw)
		if !ok || value <= 0 {
			return Layout{}, configError("%s: libraries.max_asset_bytes must be a positive integer", where)
		}
		layout.Libraries.MaxAssetBytes = value
	}
	if rawValue, exists := libraries["tool_definition_paths"]; exists {
		if err := json.Unmarshal(rawValue, &layout.ToolDefinitionPaths); err != nil {
			return Layout{}, configError("%s: libraries.tool_definition_paths must be an array of strings", where)
		}
		// From G, the folder of this file: a session reads its settings from W/.jad.
		for index, path := range layout.ToolDefinitionPaths {
			if trimmed := strings.TrimSpace(path); trimmed != "" {
				layout.ToolDefinitionPaths[index] = resolveFrom(global, trimmed)
			}
		}
	}
	return layout, nil
}

// workspaceDefaults reads a file's defaults. A relative env_file is resolved here from base,
// the folder of the file (design Q5), and a relative cwd from cwdBase, because the layers are
// merged before any path is read.
func workspaceDefaults(setting map[string]json.RawMessage, where, base, cwdBase, home string) (map[string]json.RawMessage, error) {
	defaults := map[string]json.RawMessage{}
	raw, exists := setting["defaults"]
	if !exists {
		return defaults, nil
	}
	if err := json.Unmarshal(raw, &defaults); err != nil || defaults == nil {
		return nil, configError("%s: defaults must be an object", where)
	}
	if err := checkLayoutSettings(defaults, where+": defaults"); err != nil {
		return nil, err
	}
	for key, from := range map[string]string{"env_file": base, "cwd": cwdBase} {
		value, exists := defaults[key]
		if !exists {
			continue
		}
		var path string
		if err := json.Unmarshal(value, &path); err != nil || strings.TrimSpace(path) == "" {
			return nil, configError("%s: defaults.%s must be a non-empty string", where, key)
		}
		defaults[key] = mustJSON(resolveFrom(from, expandHomeWith(strings.TrimSpace(path), home)))
	}
	// The other relative paths a defaults object can hold (hooks.env_file, the path rules of
	// session_files) resolve from base too; the session reads its settings with W/.jad as the
	// base, which is not the folder of the global setting.
	if raw, ok := defaults["hooks"]; ok {
		var hooks map[string]json.RawMessage
		if json.Unmarshal(raw, &hooks) == nil && hooks != nil && absoluteEnvFile(hooks, base) {
			defaults["hooks"] = mustJSON(hooks)
		}
	}
	if raw, ok := defaults["session_files"]; ok {
		var files map[string]json.RawMessage
		if json.Unmarshal(raw, &files) == nil && files != nil {
			if paths, ok := files["paths"]; ok {
				if rewritten, did := absolutePolicyPaths(paths, base); did {
					files["paths"] = rewritten
					defaults["session_files"] = mustJSON(files)
				}
			}
		}
	}
	return defaults, nil
}

// startWorkspace reads the start directory's workspace.json. Any problem refuses the start:
// this workspace was picked explicitly.
func startWorkspace(cwd string) (workspaceSpec, error) {
	path := filepath.Join(cwd, ".jad", workspaceSettingName)
	setting, err := readObject(path)
	if err != nil {
		return workspaceSpec{}, err
	}
	if err := checkWorkspaceFileKeys(setting, path, workspaceSettingKeys); err != nil {
		return workspaceSpec{}, err
	}
	spec := workspaceSpec{dir: filepath.Clean(cwd), real: realPath(cwd), setting: setting, start: true}
	for key, target := range map[string]*string{"id": &spec.id, "label": &spec.label} {
		raw, exists := setting[key]
		if !exists {
			continue
		}
		if err := json.Unmarshal(raw, target); err != nil || strings.TrimSpace(*target) == "" {
			return workspaceSpec{}, configError("%s: %s must be a non-empty string", path, key)
		}
		*target = strings.TrimSpace(*target)
	}
	if spec.id != "" && !rootIDPattern.MatchString(spec.id) {
		return workspaceSpec{}, configError("%s: id must match %s", path, rootIDPattern.String())
	}
	return spec, nil
}

// listedWorkspaces reads global.json workspaces: [{id, path, label}]. A path must be absolute
// or start with ~; an id must be a root id.
func listedWorkspaces(setting map[string]json.RawMessage, where, home string) ([]workspaceSpec, error) {
	raw, exists := setting["workspaces"]
	if !exists {
		return nil, nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, configError("%s: workspaces must be an array of objects with id and path", where)
	}
	specs := make([]workspaceSpec, 0, len(entries))
	for index, entry := range entries {
		at := fmt.Sprintf("%s: workspaces[%d]", where, index)
		if entry == nil {
			return nil, configError("%s must be an object with id and path", at)
		}
		for key := range entry {
			if !slices.Contains(workspaceEntryKeys, key) {
				return nil, configError("%s: %s is not a key of a workspace (id, path, label)", at, key)
			}
		}
		var spec workspaceSpec
		var path string
		for key, target := range map[string]*string{"id": &spec.id, "label": &spec.label, "path": &path} {
			value, exists := entry[key]
			if !exists {
				if key == "label" {
					continue
				}
				return nil, configError("%s: %s is required", at, key)
			}
			if err := json.Unmarshal(value, target); err != nil || strings.TrimSpace(*target) == "" {
				return nil, configError("%s: %s must be a non-empty string", at, key)
			}
			*target = strings.TrimSpace(*target)
		}
		if !rootIDPattern.MatchString(spec.id) {
			return nil, configError("%s: id must match %s", at, rootIDPattern.String())
		}
		expanded := expandHomeWith(path, home)
		if !filepath.IsAbs(expanded) {
			return nil, configError("%s: path %q must be absolute or start with ~", at, path)
		}
		spec.dir = filepath.Clean(expanded)
		spec.real = realPath(spec.dir)
		specs = append(specs, spec)
	}
	return specs, nil
}

// pickWorkspaces checks the start and listed workspaces (design Q2) and returns the ones served,
// start first, and every known workspace for the API. A listed workspace that is missing or
// whose workspace.json is broken is skipped with a warning; everything else wrong refuses the
// start.
func pickWorkspaces(start workspaceSpec, listed []workspaceSpec, globalReal, where string) ([]workspaceSpec, []Workspace, []string, error) {
	ids := map[string]string{}
	dirs := map[string]string{}
	startListed := false
	for index := range listed {
		spec := &listed[index]
		if other, taken := ids[spec.id]; taken {
			return nil, nil, nil, configError("%s: workspace id %s is listed twice (%s and %s)", where, spec.id, other, spec.dir)
		}
		if other, taken := dirs[spec.real]; taken {
			return nil, nil, nil, configError("%s: workspaces %s and %s are the same directory %s", where, other, spec.id, spec.real)
		}
		ids[spec.id], dirs[spec.real] = spec.dir, spec.id
		if spec.real == start.real {
			if start.id != "" && start.id != spec.id {
				return nil, nil, nil, configError("%s: id %s differs from the id %s it is listed under in %s", filepath.Join(start.dir, ".jad", workspaceSettingName), start.id, spec.id, where)
			}
			start.id, startListed = spec.id, true
			if start.label == "" {
				start.label = spec.label
			}
		}
	}
	if !startListed {
		if start.id == "" {
			start.id = strings.Trim(workspaceIDCleaner.ReplaceAllString(filepath.Base(start.dir), "-"), "-._")
			if len(start.id) > 64 {
				start.id = start.id[:64]
			}
			if !rootIDPattern.MatchString(start.id) {
				start.id = "workspace"
			}
		}
		if _, taken := ids[start.id]; taken {
			return nil, nil, nil, configError("%s: the start directory %s is not listed, and its id %s is taken by a listed workspace; list it in workspaces with its own id", where, start.dir, start.id)
		}
	}
	if start.label == "" {
		start.label = start.id
	}
	if err := refuseGlobalInside(start, globalReal); err != nil {
		return nil, nil, nil, err
	}
	served := []workspaceSpec{start}
	all := []Workspace{{ID: start.id, Label: start.label, Directory: start.dir, Start: true}}
	var warnings []string
	for _, spec := range listed {
		if spec.real == start.real {
			continue
		}
		if err := refuseGlobalInside(spec, globalReal); err != nil {
			return nil, nil, nil, err
		}
		if spec.label == "" {
			spec.label = spec.id
		}
		known := Workspace{ID: spec.id, Label: spec.label, Directory: spec.dir}
		path := filepath.Join(spec.dir, ".jad", workspaceSettingName)
		setting, err := readObject(path)
		if err == nil {
			err = checkWorkspaceFileKeys(setting, path, workspaceSettingKeys)
		}
		if err == nil {
			if raw, exists := setting["id"]; exists {
				var id string
				if json.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) != spec.id {
					err = configError("%s: id must be %s, the id it is listed under in %s", path, spec.id, where)
				}
			}
		}
		if err != nil {
			known.Unavailable = err.Error()
			warnings = append(warnings, fmt.Sprintf("workspace %s is skipped: %v", spec.id, err))
			all = append(all, known)
			continue
		}
		spec.setting = setting
		served = append(served, spec)
		all = append(all, known)
	}
	return served, all, warnings, nil
}

// refuseGlobalInside refuses a workspace whose .jad is G, or whose tree holds G: its tools would
// see G/auth and every other workspace's settings as project files.
func refuseGlobalInside(spec workspaceSpec, globalReal string) error {
	if PathWithin(spec.real, globalReal) {
		return configError("workspace %s (%s) contains the global folder %s; start in a project directory, not in it or above it", spec.id, spec.dir, globalReal)
	}
	return nil
}

// layout is the workspace's Layout: the shared parts of base, its own folders under W/.jad,
// and the defaults: the global ones, then the workspace's, over a cwd of W.
func (spec workspaceSpec) layout(base Layout, globalDefaults map[string]json.RawMessage, home string) (*Layout, error) {
	jad := filepath.Join(spec.dir, ".jad")
	where := filepath.Join(jad, workspaceSettingName)
	// A relative cwd in workspace.json means a folder of the workspace, not of its .jad folder.
	own, err := workspaceDefaults(spec.setting, where, jad, spec.dir, home)
	if err != nil {
		return nil, err
	}
	layout := base
	builtIn := map[string]json.RawMessage{"cwd": mustJSON(spec.dir)}
	layout.Defaults = mergeSettings(mergeSettings(builtIn, globalDefaults), own)
	layout.DefaultLayers = []SettingLayer{{Name: SettingLayerBuiltIn, Values: builtIn}, {Name: SettingLayerGlobal, Values: globalDefaults}, {Name: SettingLayerWorkspace, Values: own}}
	layout.Profiles = filepath.Join(jad, "profiles")
	layout.State = filepath.Join(jad, "state")
	layout.WorkspaceModels = filepath.Join(jad, "models")
	layout.WorkspaceTools = filepath.Join(jad, "tools")
	layout.SessionsDir = filepath.Join(jad, "sessions")
	layout.Workspace = spec.dir
	layout.WorkspaceSetting = where
	layout.ToolDefinitionPaths = append([]string(nil), base.ToolDefinitionPaths...)
	ownLibraries, err := workspaceLibraries(spec.setting, where, jad)
	if err != nil {
		return nil, err
	}
	libraries := Libraries{GlobalDir: base.GlobalDir}
	if base.Libraries != nil {
		libraries = *base.Libraries
	}
	libraries.Workspace = ownLibraries
	layout.Libraries = &libraries
	return &layout, nil
}

// defaultsEnableSandbox reports whether defaults turn a sandbox on: an object with enabled true.
// A template name or anything else counts as not enabled, so the start warning errs on the side
// of saying there is no sandbox.
func defaultsEnableSandbox(defaults map[string]json.RawMessage) bool {
	var sandbox struct {
		Enabled bool `json:"enabled"`
	}
	return json.Unmarshal(defaults["sandbox"], &sandbox) == nil && sandbox.Enabled
}
