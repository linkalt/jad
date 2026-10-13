package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// copyTree copies a directory tree.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sampleLayout copies sample/jad to a temp dir, points the session at a temp cwd that
// holds CLAUDE.md, and returns the layout and the session location.
func sampleLayout(t *testing.T) (Layout, Location, string) {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "..", "sample", "jad"), root)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte("rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(root, "sessions", "default.json")
	editJSON(t, sessionFile, func(m map[string]any) { m["cwd"] = cwd })
	// agents_dir in the toolset is relative to the session cwd
	if err := os.MkdirAll(filepath.Join(cwd, ".jad", "subagents", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".jad", "subagents", "x", "prompt.md"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := Layout{
		Profiles: filepath.Join(root, "profiles"), Models: filepath.Join(root, "models"),
		Tools: filepath.Join(root, "tools"), MCP: filepath.Join(root, "mcp"), State: filepath.Join(cwd, ".jad", "state"),
	}
	location := Location{
		Reference: "default", SessionID: "default", File: sessionFile,
		Root: Root{ID: "local", Directory: filepath.Join(root, "sessions"), GlobalJadRoot: root},
	}
	return layout, location, root
}

func editJSON(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	out, _ := json.MarshalIndent(value, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutLoadsTheSampleSession(t *testing.T) {
	layout, location, root := sampleLayout(t)
	config, err := LoadLayoutSessionConfig(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Profile != "dev/coder" || config.DefaultProfile != "dev/coder" {
		t.Fatalf("profile = %q default = %q", config.Profile, config.DefaultProfile)
	}
	if config.ModelDefault != "openai/gpt-deep" || strings.Join(config.Models, ",") != "openai/gpt-deep,openai/gpt-fast" {
		t.Fatalf("model default %q models %v", config.ModelDefault, config.Models)
	}
	if config.ModelPath != filepath.Join(root, "models", "openai", "gpt-deep.json") {
		t.Fatalf("model path = %s", config.ModelPath)
	}
	if config.Model.Name != "gpt-6-luna" {
		t.Fatalf("model file not read: %+v", config.Model)
	}
	if config.Model.PromptText != "rules" || config.Model.PromptRole != "developer" {
		t.Fatalf("prompt = %q role %q", config.Model.PromptText, config.Model.PromptRole)
	}
	wantMessages := filepath.Join(layout.State, "default", "dev", "coder", "messages.jsonl")
	if config.MessagesPath != wantMessages {
		t.Fatalf("messages = %s, want %s", config.MessagesPath, wantMessages)
	}
	if config.StoragePaths.Staging != filepath.Join(layout.State, "default", "dev", "coder", "staging") {
		t.Fatalf("staging = %s", config.StoragePaths.Staging)
	}
	if config.StoragePaths.ToolStorage != filepath.Join(config.WorkingDir, ".jad", "state", "default", "dev", "coder", "tools") {
		t.Fatalf("tool storage = %s", config.StoragePaths.ToolStorage)
	}
	if config.NativeRead == nil || config.NativeBash == nil || config.NativeSubagent == nil || config.NativeGrep == nil {
		t.Fatalf("native tools missing: read=%v bash=%v subagent=%v", config.NativeRead, config.NativeBash, config.NativeSubagent)
	}
	if config.MaxToolRounds != 500 {
		t.Fatalf("max tool rounds = %d", config.MaxToolRounds)
	}
	if strings.Join(config.ProfileTools.Allow, ",") == "" {
		t.Fatalf("profile tools.allow not carried: %+v", config.ProfileTools)
	}
}

func TestLayoutSessionCategoryIsLoaded(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	config, err := LoadLayoutSessionConfig(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Category != "work" {
		t.Fatalf("category = %q, want work (the sample session.json sets it)", config.Category)
	}
}

// A relative agents_dir entry resolves from the effective cwd, and a profile's
// settings.cwd beats the session cwd, so the profile decides which project folder it reads.
func TestLayoutAgentsDirFollowsTheProfileCwd(t *testing.T) {
	layout, location, root := sampleLayout(t)
	sessionConfig, err := LoadLayoutSessionConfig(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sessionAgents := filepath.Join(sessionConfig.WorkingDir, ".jad", "subagents")
	if got := sessionConfig.NativeSubagent.AgentDirs; len(got) != 1 || got[0] != sessionAgents {
		t.Fatalf("session cwd: agent dirs = %v, want [%s]", got, sessionAgents)
	}

	profileCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileCwd, "CLAUDE.md"), []byte("rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(profileCwd, ".jad", "subagents", "y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileCwd, ".jad", "subagents", "y", "prompt.md"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"cwd": profileCwd}
	})
	config, err := LoadLayoutSessionConfig(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(profileCwd, ".jad", "subagents")
	if config.WorkingDir != profileCwd {
		t.Fatalf("working dir = %s, want the profile cwd %s", config.WorkingDir, profileCwd)
	}
	if got := config.NativeSubagent.AgentDirs; len(got) != 1 || got[0] != want {
		t.Fatalf("profile cwd: agent dirs = %v, want [%s]", got, want)
	}
	if names := agentNames(config.NativeSubagent); len(names) != 1 || names[0] != "y" {
		t.Fatalf("agents = %v, want only the profile folder's y", names)
	}
}

func agentNames(settings *NativeSubagentSettings) []string {
	names := make([]string, 0, len(settings.Agents))
	for _, agent := range settings.Agents {
		names = append(names, agent.Name)
	}
	return names
}

func TestLayoutPicksTheMCPEnvironmentByCategory(t *testing.T) {
	layout, location, root := sampleLayout(t)
	coder, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	pathOf := func(config SessionConfig, id string) string {
		for _, server := range config.MCPServers {
			if server.ID == id {
				return server.ConfigPath
			}
		}
		return ""
	}
	if got, want := pathOf(coder, "betterwright"), filepath.Join(root, "mcp", "browser-headed", "betterwright.json"); got != want {
		t.Fatalf("coder betterwright = %q, want %q", got, want)
	}
	if got, want := pathOf(reviewer, "betterwright"), filepath.Join(root, "mcp", "browser-headless", "betterwright.json"); got != want {
		t.Fatalf("reviewer betterwright = %q, want %q", got, want)
	}
}

func TestLayoutMCPUseSettingsComeFromTheToolset(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]MCPServerReference{}
	for _, server := range config.MCPServers {
		by[server.ID] = server
	}
	if got := by["codebase-memory-mcp"].DenyTools; strings.Join(got, ",") != "ingest_traces,delete_project" {
		t.Fatalf("deny_tools = %v", got)
	}
	if by["betterwright"].Approval != ApprovalAsk {
		t.Fatalf("betterwright approval = %q", by["betterwright"].Approval)
	}
	for id, server := range by {
		if !server.Enabled {
			t.Fatalf("%s should be enabled by default", id)
		}
	}
}

// approval.default is the safety net: a tool that does not set its own approval must
// still ask. It runs on the composed setting, so a layout session keeps it.
func TestLayoutKeepsTheApprovalDefault(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	layout.Defaults = map[string]json.RawMessage{"approval": json.RawMessage(`{"default":"ask"}`)}
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeBash == nil || config.NativeBash.Approval != ApprovalAsk {
		t.Fatalf("bash approval = %+v", config.NativeBash)
	}
	if config.NativeWrite == nil || config.NativeWrite.Approval != ApprovalAsk {
		t.Fatalf("write approval = %+v", config.NativeWrite)
	}
	for _, server := range config.MCPServers {
		if server.Approval != ApprovalAsk {
			t.Fatalf("mcp %s approval = %q, want ask", server.ID, server.Approval)
		}
	}
}

func TestLayoutExplicitApprovalOffBeatsTheDefault(t *testing.T) {
	layout, location, root := sampleLayout(t)
	layout.Defaults = map[string]json.RawMessage{"approval": json.RawMessage(`{"default":"ask"}`)}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["native"].(map[string]any)["bash"] = map[string]any{"approval": "off"}
		m["mcp"].(map[string]any)["code/zvec-grep"] = map[string]any{"approval": "off"}
	})
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeBash.Approval != ApprovalOff {
		t.Fatalf("explicit bash off lost: %q", config.NativeBash.Approval)
	}
	for _, server := range config.MCPServers {
		if server.ID == "zvec-grep" && server.Approval != ApprovalOff {
			t.Fatalf("explicit mcp off lost: %q", server.Approval)
		}
		if server.ID == "codebase-memory-mcp" && server.Approval != ApprovalAsk {
			t.Fatalf("codebase-memory approval = %q, want ask", server.Approval)
		}
	}
}

func TestLayoutRequestedModelIsAName(t *testing.T) {
	layout, location, root := sampleLayout(t)
	config, err := LoadLayoutSessionConfig(location, layout, "dev/coder", "openai/gpt-fast")
	if err != nil {
		t.Fatal(err)
	}
	if config.ModelPath != filepath.Join(root, "models", "openai", "gpt-fast.json") {
		t.Fatalf("model path = %s", config.ModelPath)
	}
	if config.ModelDefault != "openai/gpt-deep" {
		t.Fatalf("default changed: %q", config.ModelDefault)
	}
	_, err = LoadLayoutSessionSettings(location, layout, "dev/coder", "cheap/haiku")
	var clientErr *ClientError
	if err == nil || !asClientError(err, &clientErr) {
		t.Fatalf("a model outside the profile must be a client error, got %v", err)
	}
}

func asClientError(err error, target **ClientError) bool {
	value, ok := err.(*ClientError)
	if ok {
		*target = value
	}
	return ok
}

func TestLayoutSessionScopesProfilesByCategoryAndFile(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	scope := func(items ...any) {
		editJSON(t, location.File, func(m map[string]any) {
			m["scope"].(map[string]any)["profiles"] = items
		})
	}
	scope("dev")
	if _, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", ""); err == nil ||
		!strings.Contains(err.Error(), `"review/reviewer" is not available here`) {
		t.Fatalf("reviewer outside a category scope: %v", err)
	}
	scope("dev/coder")
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err != nil {
		t.Fatalf("a file scope keeps that file: %v", err)
	}
	scope("nope")
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "scope.profiles") {
		t.Fatalf("a category with no profile must be an error naming scope.profiles: %v", err)
	}
	scope("category:dev")
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "no prefix is used") {
		t.Fatalf("the old category: prefix must be refused: %v", err)
	}
}

// A session has no model, toolset or mcp scope: the key is not part of the file format.
func TestLayoutSessionHasNoScopeExceptProfiles(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	for _, key := range []string{"models", "mcp", "toolsets", "tools"} {
		editJSON(t, location.File, func(m map[string]any) {
			m["scope"].(map[string]any)[key] = []any{"openai"}
		})
		if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("scope.%s must be refused by name: %v", key, err)
		}
		editJSON(t, location.File, func(m map[string]any) {
			delete(m["scope"].(map[string]any), key)
		})
	}
}

// Every model of every category is available to a profile, and a profile narrows it.
func TestLayoutProfileSeesAllModelsUnlessItNarrows(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		delete(m, "models")
		delete(m, "model_default")
	})
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(config.Models, ","); got != "cheap/haiku,openai/gpt-deep,openai/gpt-fast" {
		t.Fatalf("models = %s", got)
	}
	if config.ModelDefault != "cheap/haiku" {
		t.Fatalf("default should be the first ID: %q", config.ModelDefault)
	}
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["models"] = []any{"openai/nope"}
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", ""); err == nil || !strings.Contains(err.Error(), `"openai/nope" is not available here`) {
		t.Fatalf("a model that does not exist must be named: %v", err)
	}
}

func TestLayoutAReferenceWithoutACategoryIsRefused(t *testing.T) {
	layout, location, root := sampleLayout(t)
	if _, err := LoadLayoutSessionSettings(location, layout, "coder", ""); err == nil || !strings.Contains(err.Error(), "must be written <category>/<name>") {
		t.Fatalf("a bare profile name: %v", err)
	}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		mcp := m["mcp"].(map[string]any)
		delete(mcp, "browser-headed/betterwright")
		mcp["betterwright"] = map[string]any{}
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "must be written <category>/<name>") {
		t.Fatalf("a bare mcp name: %v", err)
	}
}

func TestLayoutRefusesTwoMCPFilesWithTheSameName(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["mcp"].(map[string]any)["browser-headless/betterwright"] = map[string]any{}
	})
	_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), "browser-headed/betterwright") || !strings.Contains(err.Error(), "browser-headless/betterwright") ||
		!strings.Contains(err.Error(), "same tool names") {
		t.Fatalf("both environments in one toolset: %v", err)
	}
}

func TestLayoutRefusesSettingsThatHaveTheirOwnPlace(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["settings"].(map[string]any)["prompt_documents"] = []string{"X.md"}
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "prompt_documents") {
		t.Fatalf("prompt_documents in profile settings must be refused: %v", err)
	}
}

func TestLayoutRefusesAnUnknownKeyInACatalogFile(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) { m["toolsett"] = "full" })
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), "toolsett") {
		t.Fatalf("typo must be refused: %v", err)
	}
}

func TestLayoutSessionOverridesDefaultsAndProfileOverridesSession(t *testing.T) {
	layout, location, root := sampleLayout(t)
	layout.Defaults = map[string]json.RawMessage{
		"max_tool_rounds": json.RawMessage("10"),
		"hooks":           json.RawMessage(`{"enabled":false,"timeout_ms":1000}`),
	}
	editJSON(t, location.File, func(m map[string]any) {
		m["settings"] = map[string]any{"max_tool_rounds": 20}
	})
	config, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxToolRounds != 20 {
		t.Fatalf("session should beat defaults: %d", config.MaxToolRounds)
	}
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"max_tool_rounds": 30}
	})
	config, err = LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxToolRounds != 30 {
		t.Fatalf("profile should beat session: %d", config.MaxToolRounds)
	}
}

func TestMergeSettingsMergesObjectsAndReplacesTheRest(t *testing.T) {
	base := map[string]json.RawMessage{
		"a": json.RawMessage(`{"x":1,"y":{"p":1,"q":2}}`),
		"b": json.RawMessage(`[1,2]`),
		"c": json.RawMessage(`1`),
	}
	over := map[string]json.RawMessage{
		"a": json.RawMessage(`{"y":{"q":3},"z":4}`),
		"b": json.RawMessage(`[3]`),
		"c": json.RawMessage(`{"k":1}`),
	}
	merged := mergeSettings(base, over)
	if string(merged["a"]) != `{"x":1,"y":{"p":1,"q":3},"z":4}` {
		t.Fatalf("a = %s", merged["a"])
	}
	if string(merged["b"]) != `[3]` || string(merged["c"]) != `{"k":1}` {
		t.Fatalf("b = %s c = %s", merged["b"], merged["c"])
	}
	if string(base["a"]) != `{"x":1,"y":{"p":1,"q":2}}` {
		t.Fatalf("base modified: %s", base["a"])
	}
}

// rootFromSample copies sample/jad, makes its session point at a temp cwd and loads the
// root setting.json the way the server does.
func rootFromSample(t *testing.T) (Config, string) {
	t.Helper()
	_, _, root := sampleLayout(t)
	var cwd string
	editJSON(t, filepath.Join(root, "sessions", "default.json"), func(m map[string]any) { cwd = m["cwd"].(string) })
	// State must sit under the session cwd, or the read policy refuses the bash spill folder.
	editJSON(t, filepath.Join(root, "setting.json"), func(m map[string]any) {
		delete(m["libraries"].(map[string]any), "tool_definition_paths")
		m["catalogs"].(map[string]any)["state"] = filepath.Join(cwd, ".jad", "state")
	})
	config, err := Load(LoadOptions{WorkingDir: root, RootSettingPath: filepath.Join(root, "setting.json"), Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return config, root
}

func TestRootSettingGivesTheSessionRootALayout(t *testing.T) {
	config, root := rootFromSample(t)
	layout := config.Roots[0].Layout
	if layout == nil {
		t.Fatal("root has no layout")
	}
	if layout.Profiles != filepath.Join(root, "profiles") || !strings.HasSuffix(layout.State, filepath.Join(".jad", "state")) {
		t.Fatalf("folders = %s, %s", layout.Profiles, layout.State)
	}
	if layout.Defaults["max_tool_rounds"] == nil {
		t.Fatalf("defaults not read: %v", layout.Defaults)
	}
}

func TestRootSettingCatalogsAreReadAndChecked(t *testing.T) {
	_, _, root := sampleLayout(t)
	settingPath := filepath.Join(root, "setting.json")
	editJSON(t, settingPath, func(m map[string]any) {
		m["catalogs"] = map[string]any{"profiles": "p", "models": "/abs/m"}
	})
	config, err := Load(LoadOptions{WorkingDir: root, RootSettingPath: settingPath, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	layout := config.Roots[0].Layout
	if layout.Profiles != filepath.Join(root, "p") || layout.Models != "/abs/m" {
		t.Fatalf("catalogs = %+v", layout)
	}
	editJSON(t, settingPath, func(m map[string]any) {
		delete(m, "catalogs")
	})
	config, err = Load(LoadOptions{WorkingDir: root, RootSettingPath: settingPath, Environment: map[string]string{}})
	if err != nil || config.Roots[0].Layout.MCP != filepath.Join(root, ".jad", "mcp") {
		t.Fatalf("without catalogs the folders are .jad/<catalog>: %+v, err = %v", config.Roots[0].Layout, err)
	}
	editJSON(t, settingPath, func(m map[string]any) { m["catalogs"] = map[string]any{"toolsets": "x"} })
	if _, err := Load(LoadOptions{WorkingDir: root, RootSettingPath: settingPath, Environment: map[string]string{}}); err == nil || !strings.Contains(err.Error(), "catalogs.toolsets") {
		t.Fatalf("unknown catalog error = %v", err)
	}
	editJSON(t, settingPath, func(m map[string]any) {
		delete(m, "catalogs")
		m["defaults"] = map[string]any{"prompt_documents": []string{"X.md"}}
	})
	if _, err := Load(LoadOptions{WorkingDir: root, RootSettingPath: settingPath, Environment: map[string]string{}}); err == nil || !strings.Contains(err.Error(), "defaults") {
		t.Fatalf("defaults with prompt_documents error = %v", err)
	}
}

// The session loaders the server calls read the catalog layout when <id>.json exists.
func TestLoadSessionConfigUsesTheLayoutWhenSessionJSONExists(t *testing.T) {
	config, _ := rootFromSample(t)
	location, err := Resolve(config.Roots, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !location.UsesLayout() || filepath.Base(location.SettingFile()) != "default.json" {
		t.Fatalf("layout = %v, file = %s", location.UsesLayout(), location.SettingFile())
	}
	loaded, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Profile != "dev/coder" || loaded.ModelDefault != "openai/gpt-deep" {
		t.Fatalf("profile = %q, model = %q", loaded.Profile, loaded.ModelDefault)
	}
	if loaded.Model.Name == "" {
		t.Fatal("the model file was not read")
	}

}

func TestLayoutSetActiveProfileWritesSessionJSONOnly(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	profileFile := filepath.Join(root, "profiles", "review", "reviewer.json")
	before, _ := os.ReadFile(profileFile)

	update, err := SetLayoutActiveProfile(location, "review/reviewer", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if !update.Changed || update.ActiveProfile != "review/reviewer" || strings.Join(update.Profiles, ",") != "dev/coder,review/reviewer" {
		t.Fatalf("update = %+v", update)
	}
	info, err := ReadLayoutProfileInfo(location)
	if err != nil || info.ActiveProfile != "review/reviewer" {
		t.Fatalf("info = %+v, err = %v", info, err)
	}
	if after, _ := os.ReadFile(profileFile); string(after) != string(before) {
		t.Fatal("setting the active profile changed a profile file")
	}
	if again, err := SetLayoutActiveProfile(location, "review/reviewer", "turn"); err != nil || again.Changed {
		t.Fatalf("second set = %+v, err = %v", again, err)
	}
	for _, bad := range []string{"coder", "dev/missing", "other/x"} {
		var client *ClientError
		if _, err := SetLayoutActiveProfile(location, bad, "turn"); err == nil || !asClientError(err, &client) {
			t.Fatalf("profile %q error = %v, want a client error", bad, err)
		}
	}
	// the written file is still a valid session
	if _, err := LoadSessionConfig(location, "", ""); err != nil {
		t.Fatal(err)
	}
}

// The model route edits the profile file: its model_default changes, every other key keeps
// its place and the file keeps its indentation. A session that uses the profile sees it.
func TestLayoutSetModelDefaultWritesTheProfileFile(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	profileFile := filepath.Join(root, "profiles", "dev", "coder.json")
	sessionFile := filepath.Join(root, "sessions", "default.json")
	profileBefore, _ := os.ReadFile(profileFile)
	sessionBefore, _ := os.ReadFile(sessionFile)

	update, err := SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if !update.Changed || update.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("update = %+v", update)
	}
	loaded, err := LoadSessionSettings(location, "dev/coder", "")
	if err != nil || loaded.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("model default = %q, err = %v", loaded.ModelDefault, err)
	}
	if after, _ := os.ReadFile(sessionFile); string(after) != string(sessionBefore) {
		t.Fatal("choosing a model changed session.json")
	}
	want := strings.Replace(string(profileBefore), `"model_default": "openai/gpt-deep"`, `"model_default": "openai/gpt-fast"`, 1)
	if after, _ := os.ReadFile(profileFile); string(after) != want {
		t.Fatalf("profile file after:\n%s\nwant:\n%s", after, want)
	}
	if again, _ := SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "turn"); again.Changed {
		t.Fatal("setting the same model twice reported a change")
	}
	// another profile keeps its own default
	other, err := LoadSessionSettings(location, "review/reviewer", "")
	if err != nil || other.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("reviewer model = %q, err = %v", other.ModelDefault, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "profiles", "dev", "*.tmp-*")); len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// A profile without model_default gets the key right after models, the file mode stays,
// and a tab-indented file stays tab-indented.
func TestLayoutSetModelDefaultAddsTheKeyAfterModelsAndKeepsTheMode(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	profileFile := filepath.Join(root, "profiles", "dev", "coder.json")
	text := "{\n\t\"schema_version\": 1,\n\t\"models\": [\n\t\t\"openai\"\n\t],\n\t\"toolset\": \"coding/full\"\n}\n"
	if err := os.WriteFile(profileFile, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(profileFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "strict"); err != nil {
		t.Fatal(err)
	}
	want := "{\n\t\"schema_version\": 1,\n\t\"models\": [\n\t\t\"openai\"\n\t],\n\t\"model_default\": \"openai/gpt-fast\",\n\t\"toolset\": \"coding/full\"\n}\n"
	if after, _ := os.ReadFile(profileFile); string(after) != want {
		t.Fatalf("profile file after:\n%q\nwant:\n%q", after, want)
	}
	if info, _ := os.Stat(profileFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

// A model the profile does not offer, or a profile outside the session's scope, is a client
// error and writes nothing.
func TestLayoutSetModelDefaultRefusesWhatIsNotOffered(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	profileFile := filepath.Join(root, "profiles", "dev", "coder.json")
	before, _ := os.ReadFile(profileFile)
	for name, tc := range map[string]struct{ profile, model, want string }{
		"model not offered":    {"dev/coder", "cheap/haiku", "is not offered by profile"},
		"unknown model":        {"dev/coder", "openai/nope", "is not offered by profile"},
		"profile not in scope": {"other/x", "openai/gpt-fast", "is not available to this session"},
		"blank model":          {"dev/coder", "  ", "model_default must be a non-empty string"},
	} {
		var client *ClientError
		_, err := SetLayoutModelDefault(location, tc.profile, tc.model, "turn")
		if err == nil || !asClientError(err, &client) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want a client error containing %q", name, err, tc.want)
		}
	}
	if after, _ := os.ReadFile(profileFile); string(after) != string(before) {
		t.Fatal("a refused update changed the profile file")
	}
}

// The profile file changed between the read and the rename (here: by the hook the test
// installs before the write): nothing is written and the caller gets a conflict.
func TestWriteFileIfUnchangedReportsAConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	later := before.ModTime().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("{\"x\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	err := writeFileIfUnchanged(path, []byte("{\"mine\":true}\n"), before, false)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want a ConflictError", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "{\"x\":1}\n" {
		t.Fatalf("the other writer's file was overwritten: %q", got)
	}
	if leftovers, _ := filepath.Glob(path + ".tmp-*"); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
	// With the file as it was read, the write goes through.
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, _ := os.Stat(path)
	if err := writeFileIfUnchanged(path, []byte("{\"mine\":true}\n"), fresh, false); err != nil {
		t.Fatalf("unchanged file: %v", err)
	}
}

// An old session.json that still has model_defaults is refused, by the loader and by the
// model route, with a message that says where the value goes now. Nothing is written.
func TestLayoutRefusesTheRemovedModelDefaultsKey(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	sessionFile := filepath.Join(root, "sessions", "default.json")
	editJSON(t, sessionFile, func(m map[string]any) {
		m["model_defaults"] = map[string]any{"dev/coder": "openai/gpt-fast"}
	})
	before, _ := os.ReadFile(sessionFile)
	profileBefore, _ := os.ReadFile(filepath.Join(root, "profiles", "dev", "coder.json"))
	want := []string{"model_defaults was removed", `{"dev/coder":"openai/gpt-fast"}`, "profile file's model_default"}
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		for _, text := range want {
			if !strings.Contains(err.Error(), text) {
				t.Errorf("%s: error %q lacks %q", what, err, text)
			}
		}
	}
	_, err := LoadSessionSettings(location, "dev/coder", "")
	check("load", err)
	_, err = SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "turn")
	check("model route", err)
	_, err = SetLayoutActiveProfile(location, "review/reviewer", "turn")
	check("profile route", err)
	if after, _ := os.ReadFile(sessionFile); string(after) != string(before) {
		t.Fatal("a refused call changed session.json")
	}
	if after, _ := os.ReadFile(filepath.Join(root, "profiles", "dev", "coder.json")); string(after) != string(profileBefore) {
		t.Fatal("a refused call changed the profile file")
	}
}

// cwd may come from the root defaults, the session or the profile; the nearer level wins.
func TestLayoutCWDComesFromDefaultsThenSessionThenProfile(t *testing.T) {
	layout, location, root := sampleLayout(t)
	fromDefaults, fromSettings, fromProfile := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{fromDefaults, fromSettings, fromProfile} {
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("rules"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".jad", "subagents", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".jad", "subagents", "x", "prompt.md"), []byte("p"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sessionCWD := ""
	editJSON(t, location.File, func(m map[string]any) {
		sessionCWD, _ = m["cwd"].(string)
		delete(m, "cwd")
	})
	layout.Defaults = map[string]json.RawMessage{"cwd": json.RawMessage(strconv.Quote(fromDefaults))}
	load := func() string {
		t.Helper()
		config, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
		if err != nil {
			t.Fatal(err)
		}
		return config.WorkingDir
	}
	if got := load(); got != fromDefaults {
		t.Fatalf("defaults only: cwd = %q, want %q", got, fromDefaults)
	}
	editJSON(t, location.File, func(m map[string]any) { m["cwd"] = sessionCWD })
	if got := load(); got != sessionCWD {
		t.Fatalf("session top-level cwd must beat defaults: cwd = %q, want %q", got, sessionCWD)
	}
	editJSON(t, location.File, func(m map[string]any) {
		delete(m, "cwd")
		m["settings"] = map[string]any{"cwd": fromSettings}
	})
	if got := load(); got != fromSettings {
		t.Fatalf("session settings.cwd must beat defaults: cwd = %q, want %q", got, fromSettings)
	}
	editJSON(t, location.File, func(m map[string]any) { m["cwd"] = sessionCWD })
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"cwd": fromProfile}
	})
	if got := load(); got != fromProfile {
		t.Fatalf("profile cwd must beat the session: cwd = %q, want %q", got, fromProfile)
	}
	// another profile of the same session is not affected
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkingDir != sessionCWD {
		t.Fatalf("dev/coder: cwd = %q, want the session's %q", config.WorkingDir, sessionCWD)
	}
}

// A relative cwd is refused wherever it comes from.
func TestLayoutRefusesARelativeCWDInProfileSettings(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"cwd": "relative/dir"}
	})
	_, err := LoadLayoutSessionSettings(location, layout, "review/reviewer", "")
	if err == nil || !strings.Contains(err.Error(), "cwd must be an absolute path") {
		t.Fatalf("error = %v", err)
	}
}

// The schemas list the setting keys a settings object may carry; the loader has its own
// list. They must be the same list, or the schema allows what the loader refuses.
func TestLayoutSettingKeysMatchTheSchemas(t *testing.T) {
	want := make([]string, 0, len(layoutSettingKeys))
	for key := range layoutSettingKeys {
		want = append(want, key)
	}
	sort.Strings(want)
	settingData, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "setting.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settingSchema struct {
		Defs struct {
			LayoutSettings struct {
				PropertyNames struct {
					Enum []string `json:"enum"`
				} `json:"propertyNames"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"layout_settings"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(settingData, &settingSchema); err != nil {
		t.Fatal(err)
	}
	shared := settingSchema.Defs.LayoutSettings
	got := append([]string(nil), shared.PropertyNames.Enum...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("layout_settings.propertyNames.enum = %v, loader keys = %v", got, want)
	}
	described := make([]string, 0, len(shared.Properties))
	for key := range shared.Properties {
		described = append(described, key)
	}
	sort.Strings(described)
	if strings.Join(described, ",") != strings.Join(want, ",") {
		t.Errorf("layout_settings.properties = %v, loader keys = %v", described, want)
	}
	for _, file := range []struct{ name, property string }{
		{"session.schema.json", "settings"},
		{"profile.schema.json", "settings"},
		{"root-setting.schema.json", "defaults"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", file.name))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Ref string `json:"$ref"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		if ref := schema.Properties[file.property].Ref; ref != "setting.schema.json#/$defs/layout_settings" {
			t.Errorf("%s %s $ref = %q, want the shared layout_settings definition", file.name, file.property, ref)
		}
	}
}

// A relative env_file in a toolset is relative to the folder of the root setting.json (the
// global root), not to a session folder: a toolset is shared by many sessions.
func TestLayoutEnvFileIsRelativeToTheRootFolder(t *testing.T) {
	layout, location, root := sampleLayout(t)
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("FROM_ROOT=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		native := m["native"].(map[string]any)
		native["bash"] = map[string]any{"env_file": "env"}
		native["process"] = map[string]any{"env_file": "env"}
	})
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "env")
	if config.NativeBash == nil || config.NativeBash.EnvFile != want || config.NativeBash.Env["FROM_ROOT"] != "1" {
		t.Fatalf("bash env file = %+v, want %s with FROM_ROOT", config.NativeBash, want)
	}
	if config.NativeProcess == nil || config.NativeProcess.EnvFile != want {
		t.Fatalf("process env file = %+v, want %s", config.NativeProcess, want)
	}
	// an absolute path is left alone
	other := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(other, []byte("OTHER=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["native"].(map[string]any)["bash"] = map[string]any{"env_file": other}
	})
	if config, err = LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err != nil || config.NativeBash.EnvFile != other {
		t.Fatalf("absolute env file: %v, %+v", err, config.NativeBash)
	}
}

// A missing relative env_file names the path it looked at, so the user sees the root folder.
func TestLayoutEnvFileErrorNamesTheRootFolderPath(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["native"].(map[string]any)["bash"] = map[string]any{"env_file": "missing.env"}
	})
	_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(root, "missing.env")) {
		t.Fatalf("error = %v, want the root folder path", err)
	}
}

// exec's own relative env_file in a toolset is relative to the root folder as well, and
// the layout loader hands exec the environment built from it.
func TestLayoutExecEnvFileIsRelativeToTheRootFolder(t *testing.T) {
	layout, location, root := sampleLayout(t)
	for name, content := range map[string]string{"bash.env": "WHO=bash\n", "exec.env": "WHO=exec\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		native := m["native"].(map[string]any)
		native["bash"] = map[string]any{"env_file": "bash.env"}
		native["exec"] = map[string]any{"env_file": "exec.env"}
	})
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeExec == nil || config.NativeExec.Child.EnvFile != filepath.Join(root, "exec.env") || config.NativeExec.Child.Env["WHO"] != "exec" {
		t.Fatalf("exec = %+v", config.NativeExec)
	}
	if config.NativeBash.Env["WHO"] != "bash" {
		t.Fatalf("bash env = %v", config.NativeBash.Env)
	}
}

// defaults.env_file is the env_file of bash, process and exec when the toolset names none; a
// tool's own env_file wins, and a tool the toolset lacks is not added.
func TestLayoutDefaultEnvFileFeedsBashProcessAndExec(t *testing.T) {
	layout, location, root := sampleLayout(t)
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("FROM_DEFAULT=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(t.TempDir(), "own.env")
	if err := os.WriteFile(own, []byte("OWN=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		native := m["native"].(map[string]any)
		native["bash"] = map[string]any{}
		native["process"] = map[string]any{"env_file": own}
		native["exec"] = map[string]any{}
	})
	layout.Defaults = map[string]json.RawMessage{"env_file": json.RawMessage(`"env"`)}
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "env")
	if config.NativeBash == nil || config.NativeBash.EnvFile != want || config.NativeBash.Env["FROM_DEFAULT"] != "1" {
		t.Fatalf("bash = %+v, want env file %s", config.NativeBash, want)
	}
	if config.NativeProcess == nil || config.NativeProcess.EnvFile != own {
		t.Fatalf("process = %+v, want its own %s", config.NativeProcess, own)
	}
	if config.NativeExec == nil || config.NativeExec.Child.EnvFile != want {
		t.Fatalf("exec = %+v, want env file %s", config.NativeExec, want)
	}
}

// A toolset without process gets no process from the default.
func TestLayoutDefaultEnvFileAddsNoTool(t *testing.T) {
	layout, location, root := sampleLayout(t)
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout.Defaults = map[string]json.RawMessage{"env_file": json.RawMessage(`"env"`)}
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.NativeProcess != nil || config.NativeExec != nil {
		t.Fatalf("process = %+v, exec = %+v, want neither", config.NativeProcess, config.NativeExec)
	}
}

// The nearest level wins: profile settings, then session settings, then root defaults.
func TestLayoutEnvFileNearestLevelWins(t *testing.T) {
	layout, location, root := sampleLayout(t)
	for _, name := range []string{"env", "session.env", "profile.env"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("A=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layout.Defaults = map[string]json.RawMessage{"env_file": json.RawMessage(`"env"`)}
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["native"].(map[string]any)["bash"] = map[string]any{}
	})
	check := func(label, want string) {
		t.Helper()
		config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if config.NativeBash == nil || config.NativeBash.EnvFile != filepath.Join(root, want) {
			t.Fatalf("%s: bash = %+v, want %s", label, config.NativeBash, want)
		}
	}
	check("defaults", "env")
	editJSON(t, location.File, func(m map[string]any) {
		m["settings"] = map[string]any{"env_file": "session.env"}
	})
	check("session", "session.env")
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"env_file": "profile.env"}
	})
	check("profile", "profile.env")
}

// A default that names a missing file fails and names the path it looked at.
func TestLayoutDefaultEnvFileMissingFileFails(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		m["native"].(map[string]any)["bash"] = map[string]any{}
	})
	layout.Defaults = map[string]json.RawMessage{"env_file": json.RawMessage(`"nope.env"`)}
	_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(root, "nope.env")) {
		t.Fatalf("error = %v", err)
	}
}

// Without a cwd anywhere, the layout fails and says where to set one; it does not fall back
// to the root folder.
func TestLayoutRequiresACWD(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	var cwd string
	editJSON(t, location.File, func(m map[string]any) {
		cwd = m["cwd"].(string)
		delete(m, "cwd")
	})
	_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), "cwd is required") || !strings.Contains(err.Error(), "defaults.cwd") {
		t.Fatalf("error = %v", err)
	}
	layout.Defaults = map[string]json.RawMessage{"cwd": json.RawMessage(strconv.Quote(cwd))}
	if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err != nil {
		t.Fatalf("with defaults.cwd: %v", err)
	}
}

// setAgentModel writes agent x's agent.json in the session cwd with the given model value
// (none when empty) and returns the cwd.
func setAgentModel(t *testing.T, location Location, model string) string {
	t.Helper()
	var cwd string
	editJSON(t, location.File, func(m map[string]any) { cwd = m["cwd"].(string) })
	agent := map[string]any{"description": "x"}
	if model != "" {
		agent["model"] = model
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".jad", "subagents", "x", "agent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func subagentX(t *testing.T, config SessionConfig) NativeSubagentAgent {
	t.Helper()
	if config.NativeSubagent == nil {
		t.Fatal("no sub-agent settings")
	}
	agent, ok := config.NativeSubagent.Agent("x")
	if !ok {
		t.Fatal("agent x missing")
	}
	return agent
}

// In a layout session an agent's model is a model ID of the catalog, also one the calling
// profile does not list.
func TestLayoutSubagentModelIsAnID(t *testing.T) {
	layout, location, root := sampleLayout(t)
	setAgentModel(t, location, "cheap/haiku")
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(config.Models, ","), "cheap/haiku") {
		t.Fatalf("the profile lists cheap/haiku, so this test proves nothing: %v", config.Models)
	}
	if got, want := subagentX(t, config).ModelPath, filepath.Join(root, "models", "cheap", "haiku.json"); got != want {
		t.Fatalf("agent model path = %s, want %s", got, want)
	}
}

// An unknown ID fails the load and lists what exists.
func TestLayoutSubagentUnknownModelIDFails(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	setAgentModel(t, location, "cheap/nope")
	_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), "cheap/nope") || !strings.Contains(err.Error(), "cheap/haiku") || !strings.Contains(err.Error(), "agent x") {
		t.Fatalf("error = %v", err)
	}
}

// A value ending in .json is still a file path, relative to the agent's folder.
func TestLayoutSubagentModelFilePathStillWorks(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	cwd := setAgentModel(t, location, "own.json")
	own := filepath.Join(cwd, ".jad", "subagents", "x", "own.json")
	if err := os.WriteFile(own, []byte(`{"provider":"openai","model":"m"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := subagentX(t, config).ModelPath; got != own {
		t.Fatalf("agent model path = %s, want %s", got, own)
	}
}

// No model means the calling session's model.
func TestLayoutSubagentWithoutModelKeepsTheCallersModel(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	setAgentModel(t, location, "")
	config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := subagentX(t, config).ModelPath; got != "" {
		t.Fatalf("agent model path = %q, want empty", got)
	}
}

// Tool state (bash spill, memory, tasks, fetch, processes, sub-agent runs, write/edit
// backups) lives under the session cwd, so the file tools reach it with no extra rule.
// The transcript and staging stay under the root state folder.
func TestLayoutToolStorageIsUnderTheCWD(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	layout.State = filepath.Join(t.TempDir(), "state")
	config, err := LoadLayoutSessionSettings(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(config.WorkingDir, ".jad", "state", "default", "dev", "coder", "tools")
	if config.StoragePaths.ToolStorage != want {
		t.Fatalf("tool storage = %s, want %s", config.StoragePaths.ToolStorage, want)
	}
	if !strings.HasPrefix(config.NativeBash.Spill.StorageDir, want+string(filepath.Separator)) {
		t.Fatalf("bash spill = %s, want it under %s", config.NativeBash.Spill.StorageDir, want)
	}
	if !strings.HasPrefix(config.MessagesPath, layout.State+string(filepath.Separator)) {
		t.Fatalf("messages = %s, want it under the root state %s", config.MessagesPath, layout.State)
	}
}

// tool_storage_dir sets the folder under the cwd; the nearest level wins and an absolute
// path is used as it is.
func TestLayoutToolStorageDirIsConfigurable(t *testing.T) {
	layout, location, root := sampleLayout(t)
	layout.Defaults = map[string]json.RawMessage{"tool_storage_dir": json.RawMessage(`"from-defaults"`)}
	config, err := LoadLayoutSessionSettings(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config.WorkingDir, "from-defaults", "default", "dev", "coder", "tools"); config.StoragePaths.ToolStorage != want {
		t.Fatalf("defaults: tool storage = %s, want %s", config.StoragePaths.ToolStorage, want)
	}
	abs := filepath.Join(config.WorkingDir, "abs-tools")
	editJSON(t, filepath.Join(root, "sessions", "default.json"), func(m map[string]any) {
		m["settings"].(map[string]any)["tool_storage_dir"] = abs
	})
	config, err = LoadLayoutSessionSettings(location, layout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(abs, "default", "dev", "coder", "tools"); config.StoragePaths.ToolStorage != want {
		t.Fatalf("session: tool storage = %s, want %s", config.StoragePaths.ToolStorage, want)
	}
}

// The profile's instructions come first in the prompt, then its documents, and the
// settings loader carries them to the config.
func TestLayoutProfileInstructionsComeBeforeThePromptDocuments(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["instructions"] = "You are the coder."
	})
	config, err := LoadLayoutSessionConfig(location, layout, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Model.PromptText != "You are the coder.\n\nrules" {
		t.Fatalf("PromptText = %q, want the instructions, a blank line, then CLAUDE.md", config.Model.PromptText)
	}
	if config.Model.PromptRole != "developer" {
		t.Fatalf("PromptRole = %q", config.Model.PromptRole)
	}
	// Without instructions the prompt is the documents only, as before.
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) { delete(m, "instructions") })
	config, err = LoadLayoutSessionConfig(location, layout, "dev/coder", "")
	if err != nil || config.Model.PromptText != "rules" {
		t.Fatalf("PromptText = %q, err = %v", config.Model.PromptText, err)
	}
}

// A profile with instructions and no documents still gets a prompt.
func TestLayoutProfileInstructionsWithoutDocuments(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["instructions"] = "Only this."
		delete(m, "prompt_documents")
	})
	config, err := LoadLayoutSessionConfig(location, layout, "dev/coder", "")
	if err != nil || config.Model.PromptText != "Only this." {
		t.Fatalf("PromptText = %q, err = %v", config.Model.PromptText, err)
	}
}

// An empty or non-string instructions is refused. Like model_default, it is read from the
// selected profile only.
func TestLayoutProfileInstructionsMustBeANonEmptyString(t *testing.T) {
	for name, value := range map[string]any{"empty": "", "blank": "  ", "number": 3, "null": nil, "list": []any{"a"}} {
		t.Run(name, func(t *testing.T) {
			layout, location, root := sampleLayout(t)
			editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) { m["instructions"] = value })
			_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
			if err == nil || !strings.Contains(err.Error(), "instructions must be a non-empty string") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// A model file with instructions fails to load and says where the text goes now, so the
// text a model file used to carry is never silently dropped.
func TestLayoutModelFileInstructionsAreRefused(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "models", "openai", "gpt-deep.json"), func(m map[string]any) {
		m["instructions"] = "old text"
	})
	_, err := LoadLayoutSessionConfig(location, layout, "dev/coder", "")
	if err == nil || !strings.Contains(err.Error(), "gpt-deep.json: instructions was removed from the model file; put the text in the profile's instructions") {
		t.Fatalf("error = %v", err)
	}
	// A model the profile does not use, and a request-level key of the same name, are not
	// affected by this rule.
	editJSON(t, filepath.Join(root, "models", "openai", "gpt-deep.json"), func(m map[string]any) { delete(m, "instructions") })
	editJSON(t, filepath.Join(root, "models", "cheap", "haiku.json"), func(m map[string]any) { m["instructions"] = "unused model" })
	if _, err := LoadLayoutSessionConfig(location, layout, "dev/coder", ""); err != nil {
		t.Fatalf("a model file the run does not load must not matter: %v", err)
	}
}

// session_files is a settings key of the root defaults, the session and the profile; the
// nearer level wins and objects merge key by key.
func TestLayoutSessionFilesComeFromDefaultsSessionAndProfile(t *testing.T) {
	layout, location, root := sampleLayout(t)
	load := func() SessionFileListingSettings {
		t.Helper()
		config, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
		if err != nil {
			t.Fatal(err)
		}
		return config.SessionFiles
	}
	if got := load(); !got.Enabled || got.MaxDepth != 12 || got.MaxResults != 2000 || got.IncludeHidden {
		t.Fatalf("defaults = %+v", got)
	}

	layout.Defaults = map[string]json.RawMessage{"session_files": json.RawMessage(`{"max_depth":3,"include_hidden":true,"max_results":50}`)}
	if got := load(); got.MaxDepth != 3 || !got.IncludeHidden || got.MaxResults != 50 || !got.Enabled {
		t.Fatalf("from defaults = %+v", got)
	}

	editJSON(t, location.File, func(m map[string]any) {
		m["settings"] = map[string]any{"session_files": map[string]any{"max_depth": 5, "ignore_directories": []any{"a", "b"}}}
	})
	got := load()
	if got.MaxDepth != 5 || !got.IncludeHidden || got.MaxResults != 50 || strings.Join(got.IgnoreDirectories, ",") != "a,b" {
		t.Fatalf("session over defaults = %+v (max_depth 5, include_hidden and max_results from defaults, ignore list replaced)", got)
	}

	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"session_files": map[string]any{"enabled": false}}
	})
	got = load()
	if got.Enabled || got.MaxDepth != 5 {
		t.Fatalf("profile over session = %+v (enabled false, max_depth 5 kept)", got)
	}
}

// A bad session_files value is a config error that names the key, from every level.
func TestLayoutSessionFilesErrors(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"not an object":  {`3`, "session_files"},
		"enabled string": {`{"enabled":"no"}`, "session_files.enabled must be a boolean"},
		"depth too big":  {`{"max_depth":65}`, "session_files.max_depth must be a safe integer between 0 and 64"},
		"ignore blank":   {`{"ignore_directories":[" "]}`, "session_files.ignore_directories[0] must be a non-empty string"},
	} {
		t.Run(name, func(t *testing.T) {
			layout, location, _ := sampleLayout(t)
			layout.Defaults = map[string]json.RawMessage{"session_files": json.RawMessage(tc.value)}
			_, err := LoadLayoutSessionSettings(location, layout, "dev/coder", "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
