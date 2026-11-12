package sessionfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"jad/backend-go/internal/catalog"
)

// The planner of "jad migrate workspace" (docs/design_workspace_layout.md §5, phase 4a). It
// reads the one v1 setting the workspace W loads today, through the v1 loader, and plans the
// copy into the workspace layout: profiles, sessions and state into W/.jad, models, tools,
// mcp, remotes and sandbox templates into G, workspace.json and (when it is missing) G's
// global.json. It writes nothing: no file, folder, lock or temporary. Whatever it cannot map
// faithfully is a blocker, not a guess.

// MigrateOptions are the inputs of PlanWorkspaceMigration.
type MigrateOptions struct {
	// WorkspaceDir is W, the exact folder the workspace is started in.
	WorkspaceDir string
	// GlobalDir is G as LoadOptions.GlobalDir names it; empty means JAD_GLOBAL_DIR, then
	// $HOME/.jad.
	GlobalDir string
	// Root is the v1 session root to migrate; needed when the setting has several.
	Root string
	// Environment replaces the process environment (tests).
	Environment map[string]string
	// LibraryDefaults are the v1 library folders a setting falls back to, by libraries key
	// (contentlibrary.DefaultDirectories). v2 has no such fallback for a workspace, so the
	// plan writes them into workspace.json for each key the v1 setting leaves out.
	LibraryDefaults map[string][]string
}

// Migration file actions.
const (
	// MigrationCopy copies the source to the destination, which does not exist.
	MigrationCopy = "copy"
	// MigrationInPlace leaves a file where it is: v2 reads it from the same path.
	MigrationInPlace = "in_place"
	// MigrationSame skips a file whose destination already holds the same content.
	MigrationSame = "same"
	// MigrationCreate creates a settings file of the workspace layout.
	MigrationCreate = "create"
	// MigrationKeep keeps an existing global.json that already agrees with the v1 setting.
	MigrationKeep = "keep"
)

// MigrationPlan is what PlanWorkspaceMigration found. Blockers empty means it can be applied.
type MigrationPlan struct {
	Workspace string `json:"workspace"`
	Global    string `json:"global"`
	// Setting is the v1 root setting the plan reads.
	Setting string `json:"setting,omitempty"`
	// Root is the v1 session root whose sessions are migrated.
	Root  string          `json:"root,omitempty"`
	Roots []MigrationRoot `json:"roots,omitempty"`
	// Settings are workspace.json and global.json as the plan would write or keep them.
	Settings    []MigrationSettingFile `json:"settings,omitempty"`
	Files       []MigrationFile        `json:"files,omitempty"`
	Rewrites    []MigrationRewrite     `json:"rewrites,omitempty"`
	Secrets     []MigrationSecret      `json:"secrets,omitempty"`
	NotMigrated []MigrationNote        `json:"not_migrated,omitempty"`
	Warnings    []string               `json:"warnings,omitempty"`
	Blockers    []MigrationBlocker     `json:"blockers,omitempty"`
}

// MigrationRoot is one v1 session root and its sessions.
type MigrationRoot struct {
	ID        string   `json:"id"`
	Directory string   `json:"directory"`
	Sessions  []string `json:"sessions"`
	Selected  bool     `json:"selected"`
}

// MigrationSettingFile is a settings file of the workspace layout the plan creates or keeps.
type MigrationSettingFile struct {
	Path    string          `json:"path"`
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
}

// MigrationFile is one file of the plan. Group is sessions, profiles, state, models, tools,
// mcp, remotes or sandbox; Global marks a destination in G, which every workspace sees.
type MigrationFile struct {
	Group       string `json:"group"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Action      string `json:"action"`
	Global      bool   `json:"global,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	// Rewritten marks a copy whose relative paths are rewritten (Rewrites).
	Rewritten bool `json:"rewritten,omitempty"`
}

// MigrationRewrite is one path value the copy changes so it names the same place it named in
// v1; File is the destination, Key the JSON key path.
type MigrationRewrite struct {
	File string `json:"file"`
	Key  string `json:"key"`
	From string `json:"from"`
	To   string `json:"to"`
}

// MigrationSecret names a field that looks like a secret, by file and key; never its value.
type MigrationSecret struct {
	File string `json:"file"`
	Key  string `json:"key"`
}

// MigrationNote is something the plan leaves out, with the reason.
type MigrationNote struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// MigrationBlocker is a reason the plan cannot be applied.
type MigrationBlocker struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Blocked reports whether the plan has a blocker.
func (p MigrationPlan) Blocked() bool { return len(p.Blockers) > 0 }

// Migration blocker kinds.
const (
	migrationBlockWorkspaceExists = "workspace_exists"
	migrationBlockNoSetting       = "no_v1_setting"
	migrationBlockV1Invalid       = "v1_setting_invalid"
	migrationBlockUnknownKey      = "unknown_key"
	migrationBlockRoots           = "root_required"
	migrationBlockUnknownRoot     = "unknown_root"
	migrationBlockPathBase        = "path_base_changes"
	migrationBlockGlobalPlace     = "global_folder_place"
	migrationBlockClash           = "name_clash"
	migrationBlockAmbiguous       = "ambiguous_id"
	migrationBlockGlobalSetting   = "global_json_conflict"
	migrationBlockLink            = "symlink"
	migrationBlockStorage         = "storage_outside"
	migrationBlockUnreadable      = "unreadable"
	migrationBlockSandboxCWD      = "sandbox_catalog_cwd"
	migrationBlockTied            = "tied_to_workspace"
	migrationBlockForbidden       = "forbidden_place"
)

// v1LibraryGlobalKeys are the v1 libraries keys that belong to G's global.json in v2; the
// folder keys (libraryFolderKeys) belong to workspace.json.
var v1LibraryGlobalKeys = []string{"tool_definition_paths", "max_asset_bytes"}

type migrationPlanner struct {
	plan       *MigrationPlan
	options    MigrateOptions
	env        map[string]string
	home       string
	workspace  string
	jad        string
	global     string
	setting    string
	settingDir string
	layout     *Layout
	// globalFolders are G's catalog folders by kind (global.json catalogs or the defaults).
	globalFolders map[string]string
	// stateInPlace: the v1 state folder is W/.jad/state, so state is not copied.
	stateInPlace bool
	// toolDefsWorkspace are the v1 tool definition paths that name a place in W, as
	// workspace.json will list them (4c); the rest go to G.
	toolDefsWorkspace []string
	// config, root and sessions are the v1 load, the migrated root and its session IDs, kept
	// for the applier's v1 and v2 comparison.
	config   Config
	root     Root
	sessions []string
}

// PlanWorkspaceMigration plans the migration of the v1 setting W loads into the workspace
// layout. It reads files only. An error is an unexpected failure; anything the migration
// cannot do faithfully is a blocker of the plan.
func PlanWorkspaceMigration(options MigrateOptions) (MigrationPlan, error) {
	p, err := newMigrationPlanner(options)
	if err != nil {
		return MigrationPlan{}, err
	}
	p.run()
	sort.SliceStable(p.plan.Files, func(i, j int) bool { return p.plan.Files[i].Destination < p.plan.Files[j].Destination })
	return *p.plan, nil
}

// newMigrationPlanner resolves W, G and the environment; run then fills the plan. The applier
// (migrate_apply.go) keeps the planner for what the plan holds besides its report: the v1
// configuration and the candidate bytes of rewritten files.
func newMigrationPlanner(options MigrateOptions) (*migrationPlanner, error) {
	workspace := strings.TrimSpace(options.WorkspaceDir)
	if workspace == "" {
		var err error
		if workspace, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace folder: %w", err)
	}
	env := options.Environment
	if env == nil {
		env = processEnvironment()
	}
	global, err := globalDir(workspace, options.GlobalDir, env)
	if err != nil {
		return nil, err
	}
	plan := &MigrationPlan{Workspace: workspace, Global: global}
	return &migrationPlanner{plan: plan, options: options, env: env, home: strings.TrimSpace(env["HOME"]), workspace: workspace, jad: filepath.Join(workspace, ".jad"), global: global}, nil
}

func (p *migrationPlanner) block(kind, path, format string, args ...any) {
	p.plan.Blockers = append(p.plan.Blockers, MigrationBlocker{Kind: kind, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (p *migrationPlanner) warn(format string, args ...any) {
	p.plan.Warnings = append(p.plan.Warnings, fmt.Sprintf(format, args...))
}

func (p *migrationPlanner) leaveOut(path, format string, args ...any) {
	p.plan.NotMigrated = append(p.plan.NotMigrated, MigrationNote{Path: path, Reason: fmt.Sprintf(format, args...)})
}

func (p *migrationPlanner) run() {
	if exists, err := workspaceAt(p.workspace); err != nil || exists {
		p.block(migrationBlockWorkspaceExists, filepath.Join(p.jad, workspaceSettingName), "the workspace layout is already set up here (or the path is not a file); a migration never changes an existing workspace")
		return
	}
	globalReal, jadReal := realPath(p.global), realPath(p.jad)
	if globalReal == jadReal || PathWithin(realPath(p.workspace), globalReal) {
		p.block(migrationBlockGlobalPlace, p.global, "the global folder is %s, inside the workspace %s; a workspace may not hold G (set JAD_GLOBAL_DIR or start in a project directory)", p.global, p.workspace)
		return
	}
	if strings.TrimSpace(p.env["JAD_ROOT_SETTING_PATH"]) != "" {
		p.warn("JAD_ROOT_SETTING_PATH is set: it keeps v1 even after workspace.json exists; unset it to start the workspace layout")
	}
	setting, err := findRootSettingPath(p.workspace, "", p.env["JAD_ROOT_SETTING_PATH"], p.env["HOME"])
	if err != nil || setting == "" {
		message := "no v1 root setting is found from this folder; there is nothing to migrate"
		if err != nil {
			message = err.Error()
		}
		p.block(migrationBlockNoSetting, p.workspace, "%s", message)
		return
	}
	p.setting, p.settingDir = setting, filepath.Dir(setting)
	p.plan.Setting = setting
	config, err := Load(LoadOptions{WorkingDir: p.workspace, Environment: p.env, GlobalDir: p.options.GlobalDir})
	if err != nil {
		p.block(migrationBlockV1Invalid, setting, "the v1 setting does not load: %v", err)
		return
	}
	if len(config.Roots) == 0 || config.Roots[0].Layout == nil || filepath.Clean(config.SettingPath) != filepath.Clean(setting) {
		p.block(migrationBlockV1Invalid, setting, "the v1 setting has no catalog layout to migrate")
		return
	}
	p.layout = config.Roots[0].Layout
	p.plan.Warnings = append(p.plan.Warnings, config.Warnings...)
	raw, err := readObject(setting)
	if err != nil {
		p.block(migrationBlockV1Invalid, setting, "%v", err)
		return
	}
	p.checkSettingKeys(raw)
	if realPath(p.settingDir) != jadReal {
		// Relative paths of sessions, profiles, toolsets and defaults resolve from the v1
		// setting's folder and from W/.jad in v2. The loader resolves them in many places
		// (each tool's normalizer), with no one list of path keys to rewrite, so a setting in
		// another folder is not planned rather than copied with paths that move.
		p.block(migrationBlockPathBase, setting, "the v1 setting is in %s, not in %s: relative paths of sessions, profiles, toolsets and defaults would resolve from another folder in v2, and the loader has no one list of path keys to rewrite; move the setting to %s (and fix its relative paths) in v1 first", p.settingDir, p.jad, filepath.Join(p.jad, v1RootSettingName))
	}
	root, ok := p.selectRoot(config.Roots)
	if !ok {
		return
	}
	p.config, p.root = config, root
	p.planGlobalSetting(raw)
	p.planWorkspaceSetting(raw, root)
	sessions := p.planSessions(root)
	p.sessions = sessions
	p.planProfiles()
	p.planState(sessions)
	p.planGlobalCatalogs()
	p.checkStorage(config.Roots, root, sessions)
	p.advise()
}

// checkSettingKeys stops on a v1 key the migration does not map: nothing is dropped silently.
func (p *migrationPlanner) checkSettingKeys(raw map[string]json.RawMessage) {
	for key := range raw {
		if !slices.Contains(rootSettingKeys, key) {
			p.block(migrationBlockUnknownKey, p.setting, "%s is not a key the migration maps (known: %s)", key, strings.Join(rootSettingKeys, ", "))
		}
	}
	libraries, err := librariesObject(raw, p.setting)
	if err != nil {
		p.block(migrationBlockV1Invalid, p.setting, "%v", err)
		return
	}
	for key := range libraries {
		if !slices.Contains(libraryFolderKeys, key) && !slices.Contains(v1LibraryGlobalKeys, key) {
			p.block(migrationBlockUnknownKey, p.setting, "libraries.%s is not a key the migration maps", key)
		}
	}
}

func (p *migrationPlanner) selectRoot(roots []Root) (Root, bool) {
	var selected *Root
	want := strings.TrimSpace(p.options.Root)
	if want == "" && len(roots) == 1 {
		want = roots[0].ID
	}
	for index := range roots {
		item := MigrationRoot{ID: roots[index].ID, Directory: roots[index].Directory, Sessions: sessionIDsIn(roots[index].Directory), Selected: roots[index].ID == want}
		if item.Selected {
			selected = &roots[index]
		}
		p.plan.Roots = append(p.plan.Roots, item)
	}
	if want == "" {
		p.block(migrationBlockRoots, p.setting, "the v1 setting has %d session roots; name the one to migrate with --root (%s)", len(roots), strings.Join(rootIDs(roots), ", "))
		return Root{}, false
	}
	if selected == nil {
		p.block(migrationBlockUnknownRoot, p.setting, "--root %s is not a session root of the v1 setting (%s)", want, strings.Join(rootIDs(roots), ", "))
		return Root{}, false
	}
	p.plan.Root = selected.ID
	for _, other := range p.plan.Roots {
		if !other.Selected {
			for _, id := range other.Sessions {
				p.leaveOut(filepath.Join(other.Directory, id+sessionFileSuffix), "session of root %s, not the migrated root %s", other.ID, selected.ID)
			}
		}
	}
	return *selected, true
}

func rootIDs(roots []Root) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		ids = append(ids, root.ID)
	}
	return ids
}

func sessionIDsIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, entry := range entries {
		if id, ok := SessionFileID(entry); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// planWorkspaceSetting plans W/.jad/workspace.json: the root's ID and label (so root:session
// references keep their meaning), the v1 defaults and the v1 library folders. The v1 setting
// is in W/.jad (else the plan is blocked), so their relative paths keep their base.
func (p *migrationPlanner) planWorkspaceSetting(raw map[string]json.RawMessage, root Root) {
	fields := map[string]json.RawMessage{"schema_version": json.RawMessage(fmt.Sprint(workspaceSchemaVersion)), "id": mustJSON(root.ID)}
	order := []string{"schema_version", "id", "label", "defaults", "libraries"}
	if root.Label != "" {
		fields["label"] = mustJSON(root.Label)
	}
	if defaults, ok := raw["defaults"]; ok {
		fields["defaults"] = defaults
	}
	p.pinToolStorage(fields)
	libraries, _ := librariesObject(raw, p.setting)
	own := map[string]json.RawMessage{}
	for _, key := range libraryFolderKeys {
		if value, ok := libraries[key]; ok {
			own[key] = value
		} else if fallback, ok := p.options.LibraryDefaults[key]; ok && len(fallback) > 0 {
			own[key] = mustJSON(fallback)
		}
	}
	if len(p.toolDefsWorkspace) > 0 {
		own["tool_definition_paths"] = mustJSON(p.toolDefsWorkspace)
	}
	if len(own) > 0 {
		encoded, err := marshalOrderedObject(own, append(append([]string(nil), libraryFolderKeys...), "tool_definition_paths"))
		if err != nil {
			p.block(migrationBlockV1Invalid, p.setting, "libraries: %v", err)
			return
		}
		fields["libraries"] = encoded
	}
	path := filepath.Join(p.jad, workspaceSettingName)
	content, err := indentedObject(fields, order)
	if err != nil {
		p.block(migrationBlockV1Invalid, p.setting, "%v", err)
		return
	}
	p.plan.Settings = append(p.plan.Settings, MigrationSettingFile{Path: path, Action: MigrationCreate, Content: content})
	p.scanSecrets(path, content)
}

// planGlobalSetting plans G/global.json from the v1 server, cli and the global library keys.
// A global.json that exists is kept only when it already agrees; otherwise v1 server
// settings would be dropped without a word, so the plan stops.
func (p *migrationPlanner) planGlobalSetting(raw map[string]json.RawMessage) {
	wanted := map[string]json.RawMessage{}
	if server, ok := raw["server"]; ok {
		rewritten, err := p.serverPaths(server)
		if err != nil {
			p.block(migrationBlockV1Invalid, p.setting, "server: %v", err)
		} else {
			wanted["server"] = rewritten
		}
	}
	if cli, ok := raw["cli"]; ok {
		wanted["cli"] = cli
	}
	libraries, _ := librariesObject(raw, p.setting)
	globalLibraries := map[string]json.RawMessage{}
	if value, ok := libraries["tool_definition_paths"]; ok {
		if paths, ok := p.splitToolDefinitions(value); ok && len(paths) > 0 {
			globalLibraries["tool_definition_paths"] = mustJSON(paths)
		}
	}
	if value, ok := libraries["max_asset_bytes"]; ok {
		globalLibraries["max_asset_bytes"] = value
	}
	if len(globalLibraries) > 0 {
		encoded, err := marshalOrderedObject(globalLibraries, v1LibraryGlobalKeys)
		if err == nil {
			wanted["libraries"] = encoded
		}
	}

	path := filepath.Join(p.global, globalSettingName)
	existing, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.globalFolders = map[string]string{}
		for _, key := range globalCatalogKeys {
			p.globalFolders[key] = filepath.Join(p.global, key)
		}
		if len(wanted) == 0 {
			return
		}
		fields := map[string]json.RawMessage{"schema_version": json.RawMessage(fmt.Sprint(workspaceSchemaVersion))}
		for key, value := range wanted {
			fields[key] = value
		}
		content, err := indentedObject(fields, []string{"schema_version", "server", "cli", "libraries"})
		if err != nil {
			p.block(migrationBlockV1Invalid, p.setting, "%v", err)
			return
		}
		p.plan.Settings = append(p.plan.Settings, MigrationSettingFile{Path: path, Action: MigrationCreate, Content: content})
		p.scanSecrets(path, content)
		p.blockTied(path, content)
		return
	case err != nil || !existing.Mode().IsRegular():
		p.block(migrationBlockGlobalSetting, path, "global.json is not a readable regular file")
		return
	}
	setting, err := readObject(path)
	if err == nil {
		err = checkWorkspaceFileKeys(setting, path, globalSettingKeys)
	}
	var layout Layout
	if err == nil {
		layout, err = globalLayout(setting, path, p.global)
	}
	if err != nil {
		p.block(migrationBlockGlobalSetting, path, "global.json does not load: %v", err)
		return
	}
	p.globalFolders = map[string]string{"models": layout.Models, "tools": layout.Tools, "mcp": layout.MCP, "remotes": layout.Remotes, "sandbox": layout.Sandbox}
	if layout.SandboxFromCWD != "" {
		p.globalFolders["sandbox"] = ""
	}
	for _, key := range []string{"server", "cli", "libraries"} {
		value, ok := wanted[key]
		if !ok {
			continue
		}
		current := setting[key]
		if key == "libraries" {
			current = subObject(current, v1LibraryGlobalKeys)
		}
		if !sameJSON(value, current) {
			p.block(migrationBlockGlobalSetting, path, "global.json exists and its %s differs from the v1 setting's; make them agree by hand (the migration never edits global.json)", key)
		}
	}
	p.plan.Settings = append(p.plan.Settings, MigrationSettingFile{Path: path, Action: MigrationKeep})
}

// pinToolStorage keeps the default tool storage where v1 puts it (decider 4c). v2 layers an
// existing G/global.json's defaults under the workspace's; if they set tool_storage_dir and the
// v1 defaults do not, a session with no value of its own would write elsewhere in v2. The
// migration then writes v1's real default, .jad/state under the session's working folder, into
// the workspace defaults. Only this key has a default a write target comes from; any other
// default a global.json adds that changes a session shows as a difference of the comparison.
func (p *migrationPlanner) pinToolStorage(fields map[string]json.RawMessage) {
	global, err := readObject(filepath.Join(p.global, globalSettingName))
	if err != nil {
		return
	}
	globalDefaults, ok := rawObjectFields(global["defaults"])
	if _, set := globalDefaults["tool_storage_dir"]; !ok || !set {
		return
	}
	defaults, _ := rawObjectFields(fields["defaults"])
	if defaults == nil {
		defaults = map[string]json.RawMessage{}
	}
	if _, set := defaults["tool_storage_dir"]; set {
		return
	}
	defaults["tool_storage_dir"] = mustJSON(defaultToolStorageDir)
	encoded, err := marshalOrderedObject(defaults, mustKeys(fields["defaults"]))
	if err != nil {
		p.block(migrationBlockV1Invalid, p.setting, "defaults: %v", err)
		return
	}
	fields["defaults"] = encoded
	p.rewrite(filepath.Join(p.jad, workspaceSettingName), "defaults.tool_storage_dir", "(v1 default)", defaultToolStorageDir)
	p.warn("%s sets defaults.tool_storage_dir, which v2 would apply to sessions that set none; workspace.json pins v1's default %s so they keep writing where they did", filepath.Join(p.global, globalSettingName), defaultToolStorageDir)
}

// insideWorkspace reports whether path names a place in W, as written or after links.
func (p *migrationPlanner) insideWorkspace(path string) bool {
	return PathWithin(p.workspace, path) || PathWithin(realPath(p.workspace), realPath(path))
}

// splitToolDefinitions sorts the v1 libraries.tool_definition_paths (decider 4c). A path that
// names a place in W goes to workspace.json (p.toolDefsWorkspace): as written when it was
// relative (both files resolve it from W/.jad), relative to W/.jad when it was an absolute path
// there, absolute for a place in W outside .jad. The others stay in G's list, absolute from
// the v1 setting's folder. A path v2 refuses to read tool definitions from (the folders a
// profile writes, G/auth) is a blocker. It returns G's list.
func (p *migrationPlanner) splitToolDefinitions(value json.RawMessage) ([]string, bool) {
	var paths []string
	if err := json.Unmarshal(value, &paths); err != nil {
		p.block(migrationBlockV1Invalid, p.setting, "libraries.tool_definition_paths must be an array of strings")
		return nil, false
	}
	globalFile, workspaceFile := filepath.Join(p.global, globalSettingName), filepath.Join(p.jad, workspaceSettingName)
	forbidden := []string{filepath.Join(p.global, "auth"), filepath.Join(p.jad, profileOwnFolder), filepath.Join(p.jad, filepath.Dir(profileDataFolder)), filepath.Join(p.jad, "profiles")}
	var shared []string
	p.toolDefsWorkspace = nil
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		resolved := resolveFrom(p.settingDir, trimmed)
		for _, root := range forbidden {
			if within(resolved, root) || within(resolveExisting(resolved), root) {
				p.block(migrationBlockForbidden, resolved, "libraries.tool_definition_paths names %s, in %s: v2 reads no tool definitions from the hidden folders or from the folders a profile writes; move the file and plan again", resolved, root)
			}
		}
		if !p.insideWorkspace(resolved) {
			if trimmed != resolved {
				p.rewrite(globalFile, fmt.Sprintf("libraries.tool_definition_paths[%d]", len(shared)), trimmed, resolved)
			}
			shared = append(shared, resolved)
			continue
		}
		written := trimmed
		if filepath.IsAbs(trimmed) {
			if relative, err := filepath.Rel(p.jad, resolved); err == nil && PathWithin(p.jad, resolved) {
				written = relative
				p.rewrite(workspaceFile, fmt.Sprintf("libraries.tool_definition_paths[%d]", len(p.toolDefsWorkspace)), trimmed, written)
			}
		}
		p.toolDefsWorkspace = append(p.toolDefsWorkspace, written)
	}
	return shared, true
}

// serverPaths makes the relative paths the server reads from its setting's folder (log_file
// and remote_api.tokens_file, normalizeServerSettings) absolute from the v1 setting's folder,
// since global.json resolves them from G.
func (p *migrationPlanner) serverPaths(raw json.RawMessage) (json.RawMessage, error) {
	fields, ok := rawObjectFields(raw)
	if !ok {
		return nil, errors.New("must be an object")
	}
	file := filepath.Join(p.global, globalSettingName)
	changed := p.absoluteString(fields, "log_file", p.settingDir, file, "server.log_file")
	if remote, ok := rawObjectFields(fields["remote_api"]); ok && p.absoluteString(remote, "tokens_file", p.settingDir, file, "server.remote_api.tokens_file") {
		encoded, err := marshalOrderedObject(remote, mustKeys(fields["remote_api"]))
		if err != nil {
			return nil, err
		}
		fields["remote_api"], changed = encoded, true
	}
	if !changed {
		return raw, nil
	}
	return marshalOrderedObject(fields, mustKeys(raw))
}

// absoluteString replaces a relative string at key with its path from base and records it.
func (p *migrationPlanner) absoluteString(fields map[string]json.RawMessage, key, base, file, label string) bool {
	var value string
	if raw, ok := fields[key]; !ok || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" || filepath.IsAbs(strings.TrimSpace(value)) {
		return false
	}
	value = strings.TrimSpace(value)
	fields[key] = mustJSON(resolveFrom(base, value))
	p.rewrite(file, label, value, resolveFrom(base, value))
	return true
}

func (p *migrationPlanner) rewrite(file, key, from, to string) {
	p.plan.Rewrites = append(p.plan.Rewrites, MigrationRewrite{File: file, Key: key, From: from, To: to})
}

// tiedPaths lists the paths of a file the migration writes into G that name a place in this
// workspace. G is read by every workspace, so such a path ties the shared file to this one,
// and the other workspaces would read this workspace's files. A path outside the workspace
// stays absolute in G.
func (p *migrationPlanner) tiedPaths(data []byte) []pathString {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	realWorkspace := realPath(p.workspace)
	var tied []pathString
	for _, found := range pathStrings(value, "") {
		if filepath.IsAbs(found.value) && (PathWithin(p.workspace, found.value) || PathWithin(realWorkspace, realPath(found.value))) {
			tied = append(tied, found)
		}
	}
	return tied
}

// blockTied stops on a tied path of a global setting or of a remote (decider 4a.1). v2 would
// take the path from workspace.json if that file had a key for it; none of these keys
// (libraries.tool_definition_paths, server.log_file, server.remote_api.tokens_file, a remote's
// token_file) has one, and the migration never adds a key on its own, so the person chooses.
func (p *migrationPlanner) blockTied(file string, data []byte) {
	var order []string
	groups := map[string][]pathString{}
	for _, found := range p.tiedPaths(data) {
		stem := found.key
		if cut := strings.IndexByte(stem, '['); cut >= 0 {
			stem = stem[:cut]
		}
		if _, seen := groups[stem]; !seen {
			order = append(order, stem)
		}
		groups[stem] = append(groups[stem], found)
	}
	for _, stem := range order {
		found := groups[stem]
		more := ""
		if len(found) > 1 {
			more = fmt.Sprintf(" and %d more", len(found)-1)
		}
		p.block(migrationBlockTied, file, "%s is %s%s, a place in this workspace (%s); %s is read by every workspace, so it would be tied to this one, and workspace.json has no key for %s; make it name a place outside the workspace in v1 (or remove it), then plan again", found[0].key, found[0].value, more, p.workspace, file, stem)
	}
}

// warnTied notes a tied path of one shared file (a toolset, MCP server or sandbox template in
// G): the file works for this workspace and names this workspace's files for the others.
func (p *migrationPlanner) warnTied(file string, data []byte) {
	for _, found := range p.tiedPaths(data) {
		p.warn("%s: %s is %s, a place in this workspace, so this shared file is tied to this workspace", file, found.key, found.value)
	}
}

type pathString struct{ key, value string }

// pathStrings lists the string values of a decoded JSON value by key path, in key order.
func pathStrings(value any, prefix string) []pathString {
	var found []pathString
	switch typed := value.(type) {
	case string:
		found = append(found, pathString{key: prefix, value: strings.TrimSpace(typed)})
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			found = append(found, pathStrings(typed[name], path)...)
		}
	case []any:
		for index, child := range typed {
			found = append(found, pathStrings(child, fmt.Sprintf("%s[%d]", prefix, index))...)
		}
	}
	return found
}

// planSessions plans the session files of the migrated root into W/.jad/sessions.
func (p *migrationPlanner) planSessions(root Root) []string {
	destination := filepath.Join(p.jad, "sessions")
	if !p.checkLinks(root.Directory, false) {
		return nil
	}
	inPlace := realPath(root.Directory) == realPath(destination)
	entries, err := os.ReadDir(root.Directory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.block(migrationBlockUnreadable, root.Directory, "%v", err)
		return nil
	}
	var ids []string
	planned := map[string]struct{}{}
	for _, entry := range entries {
		source := filepath.Join(root.Directory, entry.Name())
		id, ok := SessionFileID(entry)
		if !ok {
			if !strings.HasPrefix(entry.Name(), ".") {
				p.leaveOut(source, "not a session file of the root")
			}
			continue
		}
		ids = append(ids, id)
		planned[entry.Name()] = struct{}{}
		p.planWorkspaceFile("sessions", source, filepath.Join(destination, entry.Name()), inPlace)
	}
	if !inPlace {
		p.noteExisting(destination, planned, "a session file already in the workspace; v2 reads it as a session")
	}
	return ids
}

// planProfiles plans the v1 profile catalog into W/.jad/profiles (a copy per workspace).
func (p *migrationPlanner) planProfiles() {
	source, destination := p.layout.Profiles, filepath.Join(p.jad, "profiles")
	inPlace := realPath(source) == realPath(destination)
	files, ok := p.catalogFiles("profiles", source)
	if !ok {
		return
	}
	planned := map[string]struct{}{}
	for _, rel := range files {
		planned[rel] = struct{}{}
		p.planWorkspaceFile("profiles", filepath.Join(source, rel), filepath.Join(destination, rel), inPlace)
	}
	if !inPlace {
		p.noteExisting(destination, planned, "a profile file already in the workspace; v2 reads it as a profile")
	}
}

// planState plans <v1 state>/<session> of each migrated session into W/.jad/state/<session>.
// v1 (catalog layout) and v2 key a profile's state the same way, <state>/<session>/<category>/
// <name> (profileStateDir), so the relative path is kept one to one.
func (p *migrationPlanner) planState(sessions []string) {
	source, destination := p.layout.State, filepath.Join(p.jad, "state")
	p.stateInPlace = realPath(source) == realPath(destination)
	if p.stateInPlace {
		return
	}
	if !p.checkLinks(source, false) {
		return
	}
	selected := map[string]struct{}{}
	for _, id := range sessions {
		selected[id] = struct{}{}
		folder := filepath.Join(source, id)
		if !p.checkLinks(folder, true) {
			continue
		}
		_ = filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					p.block(migrationBlockUnreadable, path, "%v", err)
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".jad-lock") {
				p.leaveOut(path, "a lock file; locks are not copied")
				return nil
			}
			rel, _ := filepath.Rel(source, path)
			p.planWorkspaceFile("state", path, filepath.Join(destination, rel), false)
			return nil
		})
	}
	entries, _ := os.ReadDir(source)
	for _, entry := range entries {
		if _, ok := selected[entry.Name()]; ok {
			continue
		}
		path := filepath.Join(source, entry.Name())
		if entry.Name() == mcpOAuthDir {
			p.leaveOut(path, "MCP sign-in tokens are credentials and are not copied; sign in to those MCP servers again in the workspace")
			continue
		}
		if !strings.HasPrefix(entry.Name(), ".") {
			p.leaveOut(path, "not the state of a migrated session")
		}
	}
}

// planWorkspaceFile plans one file whose destination is in W/.jad.
func (p *migrationPlanner) planWorkspaceFile(group, source, destination string, inPlace bool) {
	info, err := os.Lstat(source)
	if err != nil {
		p.block(migrationBlockUnreadable, source, "%v", err)
		return
	}
	if inPlace {
		p.plan.Files = append(p.plan.Files, MigrationFile{Group: group, Source: source, Destination: destination, Action: MigrationInPlace, Size: info.Size()})
		p.scanSecretsOf(group, destination, source)
		return
	}
	data, sum, err := readSource(source)
	if err != nil {
		p.block(migrationBlockUnreadable, source, "%v", err)
		return
	}
	file := MigrationFile{Group: group, Source: source, Destination: destination, Action: MigrationCopy, Size: int64(len(data)), SHA256: sum}
	if same, clash := p.destinationHolds(destination, data, false); clash {
		p.block(migrationBlockClash, destination, "%s already exists with other content than %s", destination, source)
		return
	} else if same {
		file.Action, file.SHA256 = MigrationSame, ""
	}
	p.plan.Files = append(p.plan.Files, file)
	p.scanSecretsData(group, destination, data)
}

// destinationHolds reports whether path holds data (same) or something else (clash). A
// rewritten copy is compared as JSON, since only its values are planned, not its bytes.
func (p *migrationPlanner) destinationHolds(path string, data []byte, asJSON bool) (same, clash bool) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, true
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, true
	}
	if bytes.Equal(existing, data) || asJSON && sameJSON(existing, data) {
		return true, false
	}
	return false, true
}

// noteExisting warns about files already in a workspace folder that the plan does not put
// there: v2 reads them too.
func (p *migrationPlanner) noteExisting(folder string, planned map[string]struct{}, reason string) {
	_ = filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		rel, _ := filepath.Rel(folder, path)
		if _, ok := planned[rel]; !ok {
			p.warn("%s: %s", path, reason)
		}
		return nil
	})
}

// globalKinds are the catalogs that move to G, with the v2 workspace folder of the kinds a
// workspace may also hold (W/.jad/models and W/.jad/tools).
var globalKinds = []struct {
	kind, workspace string
	nested          bool
}{{"models", "models", true}, {"tools", "tools", true}, {"mcp", "", true}, {"remotes", "", false}, {"sandbox", "", false}}

// planGlobalCatalogs plans the v1 models, tools, mcp, remotes and sandbox templates into G.
func (p *migrationPlanner) planGlobalCatalogs() {
	if p.globalFolders == nil {
		return // global.json is blocked; G's folders are unknown
	}
	sources := map[string]string{"models": p.layout.Models, "tools": p.layout.Tools, "mcp": p.layout.MCP, "remotes": p.layout.Remotes, "sandbox": p.layout.Sandbox}
	if p.layout.SandboxFromCWD != "" {
		p.block(migrationBlockSandboxCWD, p.setting, "catalogs.sandbox is %s/%s, a folder of each session's cwd; the migration does not plan it; set catalogs.sandbox in global.json by hand", sandboxCatalogCWD, p.layout.SandboxFromCWD)
		sources["sandbox"] = ""
	}
	for _, item := range globalKinds {
		source := sources[item.kind]
		if source == "" {
			continue
		}
		destination := p.globalFolders[item.kind]
		workspace := ""
		if item.workspace != "" {
			workspace = filepath.Join(p.jad, item.workspace)
		}
		if destination == "" {
			p.block(migrationBlockSandboxCWD, filepath.Join(p.global, globalSettingName), "global.json names a %s folder of each session's cwd; the v1 %s cannot be planned into it", item.kind, source)
			continue
		}
		var files []string
		var ok bool
		if item.nested {
			files, ok = p.catalogFiles(item.kind, source)
		} else {
			files, ok = p.flatFiles(item.kind, source)
		}
		if !ok {
			continue
		}
		p.planGlobalKind(item.kind, source, destination, workspace, files)
	}
}

func (p *migrationPlanner) planGlobalKind(kind, source, destination, workspace string, files []string) {
	switch {
	case realPath(source) == realPath(destination):
		for _, rel := range files {
			p.planInPlace(kind, filepath.Join(source, rel), true)
		}
		return
	case workspace != "" && realPath(source) == realPath(workspace):
		// v2 reads this folder as the workspace's own catalog. A copy in G would give each ID
		// two scopes, which makes every bare reference ambiguous, so the files stay workspace
		// files (move them to G by hand to share them).
		p.warn("%s: the v1 %s are already where v2 reads the workspace's own %s; they stay workspace-private, not global", source, kind, kind)
		for _, rel := range files {
			if _, err := os.Lstat(filepath.Join(destination, rel)); err == nil {
				p.block(migrationBlockAmbiguous, filepath.Join(destination, rel), "%s is in G and in the workspace; bare references to it would be ambiguous in v2", strings.TrimSuffix(filepath.ToSlash(rel), ".json"))
			}
			p.planInPlace(kind, filepath.Join(source, rel), false)
		}
		return
	}
	for _, rel := range files {
		from := filepath.Join(source, rel)
		data, sum, err := readSource(from)
		if err != nil {
			p.block(migrationBlockUnreadable, from, "%v", err)
			continue
		}
		target := filepath.Join(destination, rel)
		candidate, rewrites, err := p.globalCandidate(kind, from, data)
		if err != nil {
			p.block(migrationBlockUnreadable, from, "cannot read it to check its paths: %v", err)
			continue
		}
		if workspace != "" {
			own := filepath.Join(workspace, rel)
			same, clash := p.destinationHolds(own, data, false)
			_, inGlobal := os.Lstat(target)
			switch {
			case (same || clash) && inGlobal == nil:
				p.block(migrationBlockAmbiguous, own, "%s is in G and in the workspace; bare references to it would be ambiguous in v2", strings.TrimSuffix(filepath.ToSlash(rel), ".json"))
				continue
			case clash:
				p.block(migrationBlockClash, own, "%s already exists with other content than %s", own, from)
				continue
			case same:
				p.plan.Files = append(p.plan.Files, MigrationFile{Group: kind, Source: from, Destination: own, Action: MigrationSame, Size: int64(len(data))})
				continue
			}
		}
		file := MigrationFile{Group: kind, Source: from, Destination: target, Action: MigrationCopy, Global: true, Size: int64(len(data)), SHA256: sum, Rewritten: len(rewrites) > 0}
		if same, clash := p.destinationHolds(target, candidate, len(rewrites) > 0); clash {
			p.block(migrationBlockClash, target, "%s already exists in G with other content than %s", target, from)
			continue
		} else if same {
			file.Action, file.SHA256, file.Rewritten = MigrationSame, "", false
			rewrites = nil
		}
		for _, item := range rewrites {
			p.rewrite(target, item.Key, item.From, item.To)
		}
		p.plan.Files = append(p.plan.Files, file)
		p.scanSecretsData(kind, target, data)
		if file.Action == MigrationCopy && kind == "remotes" {
			p.blockTied(target, candidate)
		} else if file.Action == MigrationCopy {
			p.warnTied(target, candidate)
		}
	}
}

func (p *migrationPlanner) planInPlace(kind, path string, global bool) {
	info, err := os.Lstat(path)
	if err != nil {
		p.block(migrationBlockUnreadable, path, "%v", err)
		return
	}
	p.plan.Files = append(p.plan.Files, MigrationFile{Group: kind, Source: path, Destination: path, Action: MigrationInPlace, Global: global, Size: info.Size()})
	p.scanSecretsOf(kind, path, path)
}

// globalCandidate is the content a file moved to G gets: a toolset's relative paths made
// absolute from the v1 setting's folder with the loader's own list (absoluteGlobalToolsetPaths,
// which v2 applies from G), a remote's token_file from its v1 folder; other kinds have no
// path the move changes (model files have none, MCP and sandbox paths are absolute or
// resolve from the session's cwd).
func (p *migrationPlanner) globalCandidate(kind, path string, data []byte) ([]byte, []MigrationRewrite, error) {
	switch kind {
	case "tools":
		return toolsetCandidate(data, p.settingDir)
	case "remotes":
		fields, ok := rawObjectFields(data)
		if !ok {
			return nil, nil, errors.New("not a JSON object")
		}
		remote, ok := rawObjectFields(fields["remote"])
		if !ok {
			return data, nil, nil
		}
		var value string
		if json.Unmarshal(remote["token_file"], &value) != nil || strings.TrimSpace(value) == "" || filepath.IsAbs(strings.TrimSpace(value)) {
			return data, nil, nil
		}
		value = strings.TrimSpace(value)
		to := resolveFrom(filepath.Dir(path), value)
		remote["token_file"] = mustJSON(to)
		encodedRemote, err := marshalOrderedObject(remote, mustKeys(fields["remote"]))
		if err != nil {
			return nil, nil, err
		}
		fields["remote"] = encodedRemote
		encoded, err := marshalOrderedObject(fields, mustKeys(data))
		return encoded, []MigrationRewrite{{Key: "remote.token_file", From: value, To: to}}, err
	}
	return data, nil, nil
}

// toolsetCandidate applies absoluteGlobalToolsetPaths with the v1 base and reports each value
// it changed.
func toolsetCandidate(data []byte, base string) ([]byte, []MigrationRewrite, error) {
	var toolset layoutToolsetFile
	if err := json.Unmarshal(data, &toolset); err != nil {
		return nil, nil, err
	}
	fields, ok := rawObjectFields(data)
	if !ok {
		return nil, nil, errors.New("not a JSON object")
	}
	rewritten := absoluteGlobalToolsetPaths(toolset, base)
	var rewrites []MigrationRewrite
	names := make([]string, 0, len(toolset.Native))
	for name := range toolset.Native {
		names = append(names, name)
	}
	sort.Strings(names)
	changedNative := false
	for _, name := range names {
		before, _ := rawObjectFields(toolset.Native[name])
		after, _ := rawObjectFields(rewritten.Native[name])
		for _, key := range []string{"env_file", "paths"} {
			if !bytes.Equal(before[key], after[key]) {
				rewrites = append(rewrites, MigrationRewrite{Key: "native." + name + "." + key, From: compactJSON(before[key]), To: compactJSON(after[key])})
				changedNative = true
			}
		}
	}
	if !bytes.Equal(toolset.Hooks, rewritten.Hooks) {
		before, _ := rawObjectFields(toolset.Hooks)
		after, _ := rawObjectFields(rewritten.Hooks)
		rewrites = append(rewrites, MigrationRewrite{Key: "hooks.env_file", From: compactJSON(before["env_file"]), To: compactJSON(after["env_file"])})
		fields["hooks"] = rewritten.Hooks
	}
	if len(rewrites) == 0 {
		return data, nil, nil
	}
	if changedNative {
		native, ok := rawObjectFields(fields["native"])
		if !ok {
			return nil, nil, errors.New("native must be an object")
		}
		for name, value := range rewritten.Native {
			native[name] = value
		}
		encoded, err := marshalOrderedObject(native, mustKeys(fields["native"]))
		if err != nil {
			return nil, nil, err
		}
		fields["native"] = encoded
	}
	encoded, err := marshalOrderedObject(fields, mustKeys(data))
	return encoded, rewrites, err
}

// catalogFiles lists the files Scan reads in a <category>/<name>.json catalog, relative to
// dir; names Scan leaves out are reported and not copied.
func (p *migrationPlanner) catalogFiles(kind, dir string) ([]string, bool) {
	if !p.checkLinks(dir, true) {
		return nil, false
	}
	scanned, err := catalog.Scan(kind, dir)
	if err != nil {
		p.block(migrationBlockUnreadable, dir, "%v", err)
		return nil, false
	}
	for _, skipped := range scanned.Skipped() {
		p.leaveOut(skipped.Path, "the %s catalog leaves it out: %s", kind, skipped.Reason)
	}
	files := make([]string, 0, scanned.Len())
	for _, entry := range scanned.Entries() {
		rel, err := filepath.Rel(dir, entry.Path)
		if err != nil {
			continue
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, true
}

// flatFiles lists the <name>.json files of a flat catalog (remotes, sandbox templates).
func (p *migrationPlanner) flatFiles(kind, dir string) ([]string, bool) {
	if !p.checkLinks(dir, true) {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		p.block(migrationBlockUnreadable, dir, "%v", err)
		return nil, false
	}
	var files []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			p.leaveOut(filepath.Join(dir, entry.Name()), "not a %s file (<name>.json)", kind)
			continue
		}
		files = append(files, entry.Name())
	}
	return files, true
}

// checkLinks stops on a symbolic link at or below dir (deep) or among its entries: links are
// never followed or copied. A missing dir has none.
func (p *migrationPlanner) checkLinks(dir string, deep bool) bool {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		p.block(migrationBlockUnreadable, dir, "%v", err)
		return false
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		p.block(migrationBlockLink, dir, "%s is a symbolic link; the migration never follows or copies links", dir)
		return false
	}
	clean := true
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			p.block(migrationBlockUnreadable, path, "%v", err)
			clean = false
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			p.block(migrationBlockLink, path, "%s is a symbolic link; the migration never follows or copies links", path)
			clean = false
			return nil
		}
		if entry.IsDir() && path != dir && !deep {
			return filepath.SkipDir
		}
		return nil
	})
	return clean
}

// checkStorage looks at where each migrated session writes (decider 4c). A write target
// inside the session's v1 state folder is copied with the state. One outside it (the default
// tool storage under the working folder, a function library, an absolute tool_storage_dir) is
// shared by v1 and v2, never copied: it holds work, not configuration, and v1 and v2 do not
// run in one workspace at once. Both layouts resolve it to the same absolute path, which the
// comparison after the switch checks (migrate_compare.go). It is a blocker when v2 would refuse
// it: G, the .jad folder of another workspace the existing global.json lists, or the
// profile-own folder of this one. Each profile in the session's scope is loaded with the v1
// loader for its resolved write targets (SessionConfig.writeTargets).
func (p *migrationPlanner) checkStorage(roots []Root, root Root, sessions []string) {
	shared := &sharedStorage{forbidden: p.storageForbidden()}
	for _, id := range sessions {
		location, err := Resolve(roots, root.ID+":"+id)
		if err != nil {
			p.warn("session %s: %v; its storage paths are not checked", id, err)
			continue
		}
		base, err := LoadLayoutSessionSettings(location, *p.layout, "", "")
		if err != nil {
			p.warn("session %s does not load in v1 (%v); its storage paths are not checked", id, err)
			continue
		}
		allowed := filepath.Join(p.layout.State, id)
		for _, profile := range base.Profiles {
			config := base
			if profile != base.Profile {
				if config, err = LoadLayoutSessionSettings(location, *p.layout, profile, ""); err != nil {
					p.warn("session %s, profile %s does not load in v1 (%v); its storage paths are not checked", id, profile, err)
					continue
				}
			}
			p.checkTargets(id, profile, allowed, config, shared)
		}
	}
	for _, setting := range slices.Sorted(maps.Keys(shared.paths)) {
		paths := shared.paths[setting]
		more := ""
		if len(paths) > 1 {
			more = fmt.Sprintf(" and %d more", len(paths)-1)
		}
		p.warn("shared storage: %s writes %s%s, outside the state the migration copies; v1 and v2 use the same files, nothing is copied, and a rollback leaves what v2 wrote there", setting, slices.Sorted(maps.Keys(paths))[0], more)
	}
}

// sharedStorage collects the write targets outside the copied state by setting name.
type sharedStorage struct {
	forbidden []string
	paths     map[string]map[string]struct{}
}

// storageForbidden are the places v2 refuses a write target in, or that no workspace may keep
// its own storage in: G, and the .jad folder of every other workspace the existing global.json
// lists, and this workspace's profile-own folder (the write-target guard).
func (p *migrationPlanner) storageForbidden() []string {
	forbidden := []string{p.global, filepath.Join(p.jad, profileOwnFolder)}
	path := filepath.Join(p.global, globalSettingName)
	setting, err := readObject(path)
	if err != nil {
		return forbidden
	}
	listed, err := listedWorkspaces(setting, path, p.home)
	if err != nil {
		return forbidden
	}
	for _, workspace := range listed {
		if directory := filepath.Join(workspace.dir, ".jad"); realPath(directory) != realPath(p.jad) {
			forbidden = append(forbidden, directory)
		}
	}
	return forbidden
}

func (p *migrationPlanner) checkTargets(session, profile, allowed string, config SessionConfig, shared *sharedStorage) {
	for _, target := range config.writeTargets() {
		path := target.path
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			if !filepath.IsAbs(config.WorkingDir) {
				continue
			}
			path = filepath.Join(config.WorkingDir, path)
		}
		if PathWithin(allowed, path) {
			continue
		}
		resolved := resolveExisting(filepath.Clean(path))
		blocked := false
		for _, root := range shared.forbidden {
			if within(path, root) || within(resolved, root) {
				p.block(migrationBlockForbidden, path, "session %s, profile %s: %s writes %s, in %s, where v2 keeps no workspace storage; change it in v1 first or leave the session out", session, profile, target.setting, path, root)
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if shared.paths == nil {
			shared.paths = map[string]map[string]struct{}{}
		}
		if shared.paths[target.setting] == nil {
			shared.paths[target.setting] = map[string]struct{}{}
		}
		shared.paths[target.setting][path] = struct{}{}
	}
	for _, derived := range config.AgentToolsets {
		p.checkTargets(session, profile, allowed, derived, shared)
	}
}

// advise adds the notes of a plan that has no blocker of its own.
func (p *migrationPlanner) advise() {
	p.warn("start the workspace in %s itself: v2 is chosen only when the start folder holds .jad/%s", p.workspace, workspaceSettingName)
	if p.stateInPlace {
		p.warn("the state stays in %s; v1 and v2 use the same transcripts, so a run in either changes them", filepath.Join(p.jad, "state"))
	}
}

// secretWords are parts of a key name that mark a value as a possible secret. A key ending
// in _env names an environment variable, and env_file a file, so neither is a value.
var secretWords = []string{"key", "token", "secret", "password", "passwd", "authorization", "credential", "cookie"}

func (p *migrationPlanner) scanSecretsOf(group, label, path string) {
	if group == "state" || !strings.HasSuffix(path, ".json") {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	p.scanSecrets(label, data)
}

func (p *migrationPlanner) scanSecretsData(group, label string, data []byte) {
	if group == "state" || !strings.HasSuffix(label, ".json") {
		return
	}
	p.scanSecrets(label, data)
}

// scanSecrets names the keys of a JSON file that look like secrets, never their values.
func (p *migrationPlanner) scanSecrets(file string, data []byte) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	for _, key := range secretKeys(value, "", false) {
		p.plan.Secrets = append(p.plan.Secrets, MigrationSecret{File: file, Key: key})
	}
}

func secretKeys(value any, prefix string, inEnv bool) []string {
	var keys []string
	switch typed := value.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			child := typed[name]
			_, nested := child.(map[string]any)
			if !nested && (inEnv || looksSecret(name)) {
				keys = append(keys, path)
				continue
			}
			keys = append(keys, secretKeys(child, path, inEnv || strings.EqualFold(name, "env") || strings.EqualFold(name, "headers"))...)
		}
	case []any:
		for index, child := range typed {
			path := fmt.Sprintf("%s[%d]", prefix, index)
			switch child.(type) {
			case map[string]any, []any:
				keys = append(keys, secretKeys(child, path, inEnv)...)
			default:
				if inEnv {
					keys = append(keys, path)
				}
			}
		}
	}
	return keys
}

func looksSecret(name string) bool {
	lower := strings.ToLower(name)
	// A _env key names a variable, a _file key a file, a tokens key a count (max_tokens) and a
	// cache_key a cache bucket.
	if strings.HasSuffix(lower, "_env") || strings.HasSuffix(lower, "_file") || strings.Contains(lower, "tokens") || strings.HasSuffix(lower, "cache_key") {
		return false
	}
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// readSource reads a source file whole with its sha256.
func readSource(path string) ([]byte, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var buffer bytes.Buffer
	if json.Compact(&buffer, raw) != nil {
		return string(raw)
	}
	return buffer.String()
}

// subObject is the object raw holding only keys (nil when raw is not an object or has none).
func subObject(raw json.RawMessage, keys []string) json.RawMessage {
	fields, ok := rawObjectFields(raw)
	if !ok {
		return nil
	}
	kept := map[string]json.RawMessage{}
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			kept[key] = value
		}
	}
	if len(kept) == 0 {
		return nil
	}
	encoded, _ := marshalOrderedObject(kept, keys)
	return encoded
}

func mustKeys(raw json.RawMessage) []string {
	keys, _ := orderedObjectKeys(raw)
	return keys
}

// indentedObject is fields in order (other keys after), indented by two spaces, with a final
// newline.
func indentedObject(fields map[string]json.RawMessage, order []string) (json.RawMessage, error) {
	encoded, err := marshalOrderedObject(fields, order)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, encoded, "", "  "); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}
