// Package layouttest writes a workspace layout (workspace.json, global.json and the catalogs) for
// tests. It imports no production package, so the tests of sessionfs can use it too.
package layouttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Spec describes one session. Every field is optional.
type Spec struct {
	// SessionID is the session id (the file name without .json); default "session-1".
	SessionID string
	// CWD is the session's working directory; default a fresh temp dir.
	CWD string
	// Workspace is W, the start directory (its .jad holds the settings, sessions, profiles and
	// state); default a fresh folder. A test sets it to CWD to have the .jad folder inside the
	// project, as a project's own .jad is.
	Workspace string
	// Settings is the session's `settings` object.
	Settings map[string]any
	// Server is global.json's `server` object.
	Server map[string]any
	// RawSettings is Settings with values given as raw JSON, for a value that Go cannot
	// build (null, a wrong type). It is merged over Settings.
	RawSettings map[string]json.RawMessage
	// Defaults is global.json's `defaults` object.
	Defaults map[string]any
	// Models maps "<category>/<name>" to a model file. Default: one model, "openai/default".
	Models map[string]map[string]any
	// ModelRaw is the exact text of the default model file "openai/default", for a test of
	// a malformed or odd one. It replaces Models.
	ModelRaw string
	// Profiles maps "<category>/<name>" to a profile file. Default: one profile, "dev/coder",
	// which lists every model and uses the toolset when there is one.
	Profiles map[string]map[string]any
	// Native is the toolset's `native` object. With MCP, it makes the toolset "coding/default".
	Native map[string]any
	// Extension is the toolset's `extension` object; like Native it makes the toolset "coding/default".
	Extension map[string]any
	// MCP maps "<category>/<name>" to the toolset's use of that server; the server file is
	// written with a placeholder connection.
	MCP map[string]map[string]any
	// ProfileModels lists the model IDs of the default profile; the first is its default
	// model. Without it the profile lists every model, the first in sorted order the default.
	ProfileModels []string
	// ProfileExtra adds keys (tools, instructions, ...) to the default profile.
	ProfileExtra map[string]any
	// ToolDefinitionPaths is global.json's libraries.tool_definition_paths.
	ToolDefinitionPaths []string
	// MCPConfig maps "<category>/<name>" to the MCP server file's content; it replaces the
	// placeholder connection MCP writes.
	MCPConfig map[string]map[string]any
	// ToolsetExtra adds top-level keys (hooks, for one) to the generated toolset file "coding/default".
	// ToolsetRaw replaces it.
	ToolsetExtra map[string]any
	// ToolsetRaw is the exact text of the toolset file "coding/default", for a test of a
	// malformed one. It replaces Native and MCP.
	ToolsetRaw string
	// Session adds top-level keys to session.json (scope, description, ...).
	Session map[string]any
	// RootID is the id of the workspace (the first session root); default "local".
	RootID string
	// ExtraRoots are the IDs of more workspaces, each its own folder with its own .jad, listed in
	// global.json together with the first (so the workspaces trust each other as listed ones do).
	ExtraRoots []string
	// NoSession writes the settings and catalogs only, for a test that adds its own sessions
	// with AddSession.
	NoSession bool
	// SandboxCatalog is global.json catalogs.sandbox; empty leaves the default, G/sandbox.
	SandboxCatalog string
	// SandboxTemplates maps a template name to the file G/sandbox/<name>.json (the default
	// catalogs.sandbox).
	SandboxTemplates map[string]map[string]any
}

// Fixture is where Write put everything. Load it with LoadOptions{WorkingDir: Workspace,
// GlobalDir: GlobalDir}.
type Fixture struct {
	// Root is the workspace's .jad folder (W/.jad): sessions, profiles and state are folders of
	// it. The models, tools, mcp servers and sandbox templates are the shared catalogs of the
	// global folder G.
	Root string
	// Workspace is W, the start directory; GlobalDir is G.
	Workspace string
	GlobalDir string
	// WorkspaceSetting is W/.jad/workspace.json, GlobalSetting is G/global.json.
	WorkspaceSetting string
	GlobalSetting    string
	// RootSetting is the root setting.json of the retired layout v1; only WriteV1 sets it.
	RootSetting string
	SessionsDir string
	// SessionFile is <sessions dir>/<id>.json; SessionStateDir is <state>/<id>.
	SessionFile     string
	SessionStateDir string
	SessionID       string
	CWD             string
	StateDir        string
	// ModelsDir holds the model files, <category>/<name>.json; SandboxDir the sandbox templates.
	ModelsDir  string
	SandboxDir string
	// RootDirs maps every workspace ID to its sessions folder.
	RootDirs map[string]string
}

// Write creates the global folder, the workspace (and the others of ExtraRoots), the catalogs and
// the session under a temp dir.
func Write(t testing.TB, spec Spec) Fixture {
	t.Helper()
	base := t.TempDir()
	workspace := spec.Workspace
	if workspace == "" {
		workspace = filepath.Join(base, "ws")
	}
	root := filepath.Join(workspace, ".jad")
	global := filepath.Join(base, "global")
	if spec.SessionID == "" {
		spec.SessionID = "session-1"
	}
	if spec.CWD == "" {
		spec.CWD = t.TempDir()
	}
	write := func(path string, value any) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put := func(rel string, value any) { write(filepath.Join(root, rel), value) }

	stateDir := filepath.Join(root, "state")
	rootID := spec.RootID
	if rootID == "" {
		rootID = "local"
	}
	sessionsDir := filepath.Join(root, "sessions")
	rootDirs := map[string]string{rootID: sessionsDir}
	workspaceSetting := map[string]any{"schema_version": 2, "id": rootID}
	write(filepath.Join(root, "workspace.json"), workspaceSetting)
	listed := []any{}
	if len(spec.ExtraRoots) > 0 {
		listed = append(listed, map[string]any{"id": rootID, "path": workspace})
	}
	for _, id := range spec.ExtraRoots {
		other := filepath.Join(base, "ws-"+id)
		rootDirs[id] = filepath.Join(other, ".jad", "sessions")
		write(filepath.Join(other, ".jad", "workspace.json"), map[string]any{"schema_version": 2, "id": id})
		listed = append(listed, map[string]any{"id": id, "path": other})
	}
	catalogs := map[string]any{}
	sandboxDir := filepath.Join(global, "sandbox")
	if spec.SandboxCatalog != "" {
		catalogs["sandbox"] = spec.SandboxCatalog
	}
	for name, template := range spec.SandboxTemplates {
		write(filepath.Join(sandboxDir, name+".json"), template)
	}
	globalSetting := map[string]any{"schema_version": 2, "catalogs": catalogs}
	if len(listed) > 0 {
		globalSetting["workspaces"] = listed
	}
	if spec.Server != nil {
		globalSetting["server"] = spec.Server
	}
	if spec.Defaults != nil {
		globalSetting["defaults"] = spec.Defaults
	}
	if len(spec.ToolDefinitionPaths) > 0 {
		globalSetting["libraries"] = map[string]any{"tool_definition_paths": spec.ToolDefinitionPaths}
	}
	write(filepath.Join(global, "global.json"), globalSetting)

	models := spec.Models
	if spec.ModelRaw != "" {
		models = map[string]map[string]any{"openai/default": nil}
		path := filepath.Join(global, "models", "openai", "default.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(spec.ModelRaw), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if models == nil {
		models = map[string]map[string]any{"openai/default": {"provider": "openai", "model": "gpt-test", "api_key_env": "FIXTURE_KEY"}}
	}
	modelIDs := make([]string, 0, len(models))
	for id, model := range models {
		if spec.ModelRaw == "" {
			write(filepath.Join(global, "models", id+".json"), model)
		}
		modelIDs = append(modelIDs, id)
	}

	toolset := ""
	if spec.ToolsetRaw != "" {
		toolset = "coding/default"
		path := filepath.Join(global, "tools", toolset+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(spec.ToolsetRaw), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if len(spec.Native) > 0 || len(spec.Extension) > 0 || len(spec.MCP) > 0 {
		toolset = "coding/default"
		file := map[string]any{"native": withAsync(orEmpty(spec.Native))}
		if len(spec.Extension) > 0 {
			file["extension"] = spec.Extension
		}
		use := map[string]any{}
		for id, fields := range spec.MCP {
			server := map[string]any{"schema_version": 1, "transport": map[string]any{"type": "streamable_http", "url": "http://127.0.0.1:1/mcp"}}
			if configured, ok := spec.MCPConfig[id]; ok {
				server = configured
			}
			write(filepath.Join(global, "mcp", id+".json"), server)
			use[id] = orEmpty(fields)
		}
		file["mcp"] = use
		for key, value := range spec.ToolsetExtra {
			file[key] = value
		}
		write(filepath.Join(global, "tools", toolset+".json"), file)
	}

	profiles := spec.Profiles
	if profiles == nil {
		sort.Strings(modelIDs)
		listed := modelIDs
		if len(spec.ProfileModels) > 0 {
			listed = spec.ProfileModels
		}
		profile := map[string]any{"models": listed, "model_default": listed[0]}
		if toolset != "" {
			profile["toolset"] = toolset
		}
		for key, value := range spec.ProfileExtra {
			profile[key] = value
		}
		profiles = map[string]map[string]any{"dev/coder": profile}
	}
	for id, profile := range profiles {
		put("profiles/"+id+".json", profile)
		// Profiles belong to a workspace: the other workspaces get the same ones.
		for _, other := range spec.ExtraRoots {
			write(filepath.Join(base, "ws-"+other, ".jad", "profiles", id+".json"), profile)
		}
	}

	session := map[string]any{"schema_version": 1, "cwd": spec.CWD}
	if spec.Settings != nil || spec.RawSettings != nil {
		settings := map[string]any{}
		for key, value := range spec.Settings {
			settings[key] = value
		}
		for key, value := range spec.RawSettings {
			settings[key] = value
		}
		session["settings"] = settings
	}
	for key, value := range spec.Session {
		session[key] = value
	}
	if !spec.NoSession {
		sessionFile := filepath.Join(sessionsDir, spec.SessionID+".json")
		if err := os.MkdirAll(filepath.Dir(sessionFile), 0o755); err != nil {
			t.Fatal(err)
		}
		encodedSession, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionFile, encodedSession, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return Fixture{
		Root: root, Workspace: workspace, GlobalDir: global, WorkspaceSetting: filepath.Join(root, "workspace.json"), GlobalSetting: filepath.Join(global, "global.json"),
		SessionsDir: sessionsDir, SessionFile: filepath.Join(sessionsDir, spec.SessionID+".json"), SessionStateDir: filepath.Join(stateDir, spec.SessionID), SessionID: spec.SessionID, CWD: spec.CWD, StateDir: stateDir, ModelsDir: filepath.Join(global, "models"), SandboxDir: filepath.Join(global, "sandbox"), RootDirs: rootDirs,
	}
}

func orEmpty(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// AddSession writes one more session into an existing fixture's root. It gets its own
// toolset ("coding/<id>") when native is not empty, and uses the profile dev/coder, which
// Write created, so the fixture must have been written with the default profile.
func (f Fixture) AddSession(t *testing.T, id, cwd string, native map[string]any, settings map[string]any) {
	t.Helper()
	put := func(path string, value any) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profile := "dev/coder"
	if len(native) > 0 {
		toolset := "coding/" + id
		put(filepath.Join(f.GlobalDir, "tools", toolset+".json"), map[string]any{"native": withAsync(native), "mcp": map[string]any{}})
		profile = "dev/" + id
		put(filepath.Join(f.Root, "profiles", profile+".json"), map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": toolset})
	}
	session := map[string]any{"schema_version": 1, "cwd": cwd, "scope": map[string]any{"profiles": []string{profile}}}
	if settings != nil {
		session["settings"] = settings
	}
	put(filepath.Join(f.SessionsDir, id+".json"), session)
}

// AddSessionOn writes one more session that uses an existing profile (for example
// "dev/coder") and no settings.
func (f Fixture) AddSessionOn(t *testing.T, id, cwd, profile string) {
	t.Helper()
	data, err := json.MarshalIndent(map[string]any{"schema_version": 1, "cwd": cwd, "scope": map[string]any{"profiles": []string{profile}}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.SessionsDir, id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// AddSessionInRoot writes one more session into the named session root, using an existing
// profile and the given settings.
func (f Fixture) AddSessionInRoot(t *testing.T, rootID, id, cwd, profile string, settings map[string]any) {
	t.Helper()
	dir, ok := f.RootDirs[rootID]
	if !ok {
		t.Fatalf("no session root %q", rootID)
	}
	session := map[string]any{"schema_version": 1, "cwd": cwd, "scope": map[string]any{"profiles": []string{profile}}}
	if settings != nil {
		session["settings"] = settings
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// withAsync adds the async tool to a native block that enables subagent or tool_smith, as the
// loader requires; a block that already names async is returned as is.
func withAsync(native map[string]any) map[string]any {
	_, hasAsync := native["async"]
	_, hasSubagent := native["subagent"]
	_, hasToolSmith := native["tool_smith"]
	if hasAsync || (!hasSubagent && !hasToolSmith) {
		return native
	}
	out := make(map[string]any, len(native)+1)
	for key, value := range native {
		out[key] = value
	}
	out["async"] = map[string]any{}
	return out
}
