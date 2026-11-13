package layouttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// WriteV1 is Write for the retired layout v1: a root setting.json (session_roots, catalogs) with the
// catalogs beside it. Only the tests of what v1 did differently use it; they go with v1 (T-041b2).
func WriteV1(t *testing.T, spec Spec) Fixture {
	t.Helper()
	root := t.TempDir()
	if spec.SessionID == "" {
		spec.SessionID = "session-1"
	}
	if spec.CWD == "" {
		spec.CWD = t.TempDir()
	}
	put := func(rel string, value any) {
		path := filepath.Join(root, rel)
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

	stateDir := filepath.Join(root, "state")
	rootID := spec.RootID
	if rootID == "" {
		rootID = "local"
	}
	rootDirs := map[string]string{rootID: filepath.Join(root, "sessions")}
	sessionRoots := []any{map[string]any{"id": rootID, "path": rootDirs[rootID]}}
	for _, id := range spec.ExtraRoots {
		rootDirs[id] = filepath.Join(root, "sessions-"+id)
		sessionRoots = append(sessionRoots, map[string]any{"id": id, "path": rootDirs[id]})
	}
	rootSetting := map[string]any{
		"schema_version": 1,
		"session_roots":  sessionRoots,
		"catalogs":       map[string]any{"profiles": "profiles", "models": "models", "tools": "tools", "mcp": "mcp", "state": stateDir},
	}
	if spec.SandboxCatalog != "" {
		rootSetting["catalogs"].(map[string]any)["sandbox"] = spec.SandboxCatalog
	}
	for name, template := range spec.SandboxTemplates {
		put(filepath.Join(".jad", "sandbox", name+".json"), template)
	}
	if spec.Server != nil {
		rootSetting["server"] = spec.Server
	}
	if spec.Defaults != nil {
		rootSetting["defaults"] = spec.Defaults
	}
	if len(spec.ToolDefinitionPaths) > 0 {
		rootSetting["libraries"] = map[string]any{"tool_definition_paths": spec.ToolDefinitionPaths}
	}
	put("setting.json", rootSetting)

	models := spec.Models
	if spec.ModelRaw != "" {
		models = map[string]map[string]any{"openai/default": nil}
		path := filepath.Join(root, "models", "openai", "default.json")
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
			put("models/"+id+".json", model)
		}
		modelIDs = append(modelIDs, id)
	}

	toolset := ""
	if spec.ToolsetRaw != "" {
		toolset = "coding/default"
		path := filepath.Join(root, "tools", toolset+".json")
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
			put("mcp/"+id+".json", server)
			use[id] = orEmpty(fields)
		}
		file["mcp"] = use
		for key, value := range spec.ToolsetExtra {
			file[key] = value
		}
		put("tools/"+toolset+".json", file)
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
		sessionFile := filepath.Join(filepath.Join(root, "sessions"), spec.SessionID+".json")
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
		Root: root, Workspace: root, GlobalDir: root, RootSetting: filepath.Join(root, "setting.json"), SessionsDir: filepath.Join(root, "sessions"),
		SessionFile: filepath.Join(filepath.Join(root, "sessions"), spec.SessionID+".json"), SessionStateDir: filepath.Join(stateDir, spec.SessionID), SessionID: spec.SessionID, CWD: spec.CWD, StateDir: stateDir, ModelsDir: filepath.Join(root, "models"), RootDirs: rootDirs,
	}
}
