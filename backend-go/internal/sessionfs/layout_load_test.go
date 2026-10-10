package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
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
	sessionDir := filepath.Join(root, "sessions", "default")
	editJSON(t, filepath.Join(sessionDir, "session.json"), func(m map[string]any) { m["cwd"] = cwd })
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
		Reference: "default", SessionID: "default", Directory: sessionDir,
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
	if config.StoragePaths.ToolStorage != filepath.Join(layout.State, "default", "dev", "coder", "tools") {
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
		editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
		editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
			m["scope"].(map[string]any)[key] = []any{"openai"}
		})
		if _, err := LoadLayoutSessionSettings(location, layout, "dev/coder", ""); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("scope.%s must be refused by name: %v", key, err)
		}
		editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
	editJSON(t, filepath.Join(root, "sessions", "default", "session.json"), func(m map[string]any) { cwd = m["cwd"].(string) })
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

// The session loaders the server calls choose the catalog layout when session.json exists
// and the legacy reader when only setting.json does.
func TestLoadSessionConfigUsesTheLayoutWhenSessionJSONExists(t *testing.T) {
	config, _ := rootFromSample(t)
	location, err := Resolve(config.Roots, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !location.UsesLayout() || filepath.Base(location.SettingFile()) != "session.json" {
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

	legacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacy, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Location{Directory: filepath.Join(legacy, "old"), Root: config.Roots[0]}
	if old.UsesLayout() {
		t.Fatal("a directory without session.json must use the legacy reader")
	}
	if got := filepath.Base(old.SettingFile()); got != "setting.json" {
		t.Fatalf("legacy setting file = %s", got)
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
	if !update.Changed || update.ActiveProfile != "review/reviewer" || strings.Join(update.PathProfiles, ",") != "dev/coder,review/reviewer" {
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

// A profile file is shared by every session, so the chosen model goes into session.json.
func TestLayoutSetModelDefaultIsPerSession(t *testing.T) {
	config, root := rootFromSample(t)
	location, _ := Resolve(config.Roots, "default")
	profileFile := filepath.Join(root, "profiles", "dev", "coder.json")
	before, _ := os.ReadFile(profileFile)

	update, err := SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if !update.Changed || update.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("update = %+v", update)
	}
	loaded, err := LoadSessionSettings(location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("model default = %q, want the session's choice", loaded.ModelDefault)
	}
	if after, _ := os.ReadFile(profileFile); string(after) != string(before) {
		t.Fatal("choosing a model changed the profile file")
	}
	if again, _ := SetLayoutModelDefault(location, "dev/coder", "openai/gpt-fast", "turn"); again.Changed {
		t.Fatal("setting the same model twice reported a change")
	}
	// the other profile keeps its own default
	other, err := LoadSessionSettings(location, "review/reviewer", "")
	if err != nil || other.ModelDefault != "openai/gpt-fast" {
		t.Fatalf("reviewer model = %q, err = %v", other.ModelDefault, err)
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) { m["cwd"] = sessionCWD })
	if got := load(); got != sessionCWD {
		t.Fatalf("session top-level cwd must beat defaults: cwd = %q, want %q", got, sessionCWD)
	}
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
		delete(m, "cwd")
		m["settings"] = map[string]any{"cwd": fromSettings}
	})
	if got := load(); got != fromSettings {
		t.Fatalf("session settings.cwd must beat defaults: cwd = %q, want %q", got, fromSettings)
	}
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) { m["cwd"] = sessionCWD })
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
				PropertyNames struct {
					Enum []string `json:"enum"`
				} `json:"propertyNames"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		node := schema.Properties[file.property]
		got := append([]string(nil), node.PropertyNames.Enum...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s %s.propertyNames.enum = %v, loader keys = %v", file.name, file.property, got, want)
		}
		described := make([]string, 0, len(node.Properties))
		for key := range node.Properties {
			described = append(described, key)
		}
		sort.Strings(described)
		if strings.Join(described, ",") != strings.Join(want, ",") {
			t.Errorf("%s %s.properties = %v, loader keys = %v", file.name, file.property, described, want)
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

// A legacy session keeps resolving a relative env_file against its own folder, where its
// setting.json lives, even when its root has a catalog layout.
func TestLegacySessionEnvFileIsRelativeToTheSessionFolder(t *testing.T) {
	config, root := rootFromSample(t)
	dir := filepath.Join(root, "sessions", "old")
	cwd := t.TempDir()
	for name, content := range map[string]string{
		"env":          "FROM_SESSION=1\n",
		"setting.json": `{"cwd":` + strconv.Quote(cwd) + `,"tools":{"native":{"bash":{"env_file":"env"}}}}`,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a file of the same name in the root folder must not be picked up
	if err := os.WriteFile(filepath.Join(root, "env"), []byte("FROM_ROOT=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	location, err := Resolve(config.Roots, "old")
	if err != nil {
		t.Fatal(err)
	}
	if location.UsesLayout() {
		t.Fatal("expected a legacy session")
	}
	loaded, err := LoadSessionSettings(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "env"); loaded.NativeBash == nil || loaded.NativeBash.EnvFile != want || loaded.NativeBash.Env["FROM_SESSION"] != "1" {
		t.Fatalf("legacy env file = %+v, want %s", loaded.NativeBash, want)
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
	if config.NativeExec == nil || config.NativeExec.EnvFile != filepath.Join(root, "exec.env") || config.NativeExec.Env["WHO"] != "exec" {
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
	if config.NativeExec == nil || config.NativeExec.EnvFile != want {
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
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
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) { cwd = m["cwd"].(string) })
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
