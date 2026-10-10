package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if config.Profile != "coder" || config.DefaultProfile != "coder" {
		t.Fatalf("profile = %q default = %q", config.Profile, config.DefaultProfile)
	}
	if config.ModelDefault != "gpt-deep" || strings.Join(config.Models, ",") != "gpt-deep,gpt-fast" {
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
	wantMessages := filepath.Join(layout.State, "default", "coder", "messages.jsonl")
	if config.MessagesPath != wantMessages {
		t.Fatalf("messages = %s, want %s", config.MessagesPath, wantMessages)
	}
	if config.StoragePaths.Staging != filepath.Join(layout.State, "default", "coder", "staging") {
		t.Fatalf("staging = %s", config.StoragePaths.Staging)
	}
	if config.StoragePaths.ToolStorage != filepath.Join(layout.State, "default", "coder", "tools") {
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
	coder, err := LoadLayoutSessionSettings(location, layout, "coder", "")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := LoadLayoutSessionSettings(location, layout, "reviewer", "")
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
	config, err := LoadLayoutSessionSettings(location, layout, "coder", "")
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
	config, err := LoadLayoutSessionSettings(location, layout, "coder", "")
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
		m["mcp"].(map[string]any)["zvec-grep"] = map[string]any{"approval": "off"}
	})
	config, err := LoadLayoutSessionSettings(location, layout, "coder", "")
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
	config, err := LoadLayoutSessionConfig(location, layout, "coder", "gpt-fast")
	if err != nil {
		t.Fatal(err)
	}
	if config.ModelPath != filepath.Join(root, "models", "openai", "gpt-fast.json") {
		t.Fatalf("model path = %s", config.ModelPath)
	}
	if config.ModelDefault != "gpt-deep" {
		t.Fatalf("default changed: %q", config.ModelDefault)
	}
	_, err = LoadLayoutSessionSettings(location, layout, "coder", "haiku")
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

func TestLayoutSessionScopeNarrowsAndReportsOutsideNames(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
		m["scope"].(map[string]any)["profiles"] = []any{"coder"}
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "reviewer", ""); err == nil ||
		!strings.Contains(err.Error(), `profile "reviewer" is not in the session scope`) {
		t.Fatalf("reviewer outside scope: %v", err)
	}
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
		m["scope"].(map[string]any)["models"] = []any{"nope"}
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "coder", ""); err == nil || !strings.Contains(err.Error(), "scope.models") {
		t.Fatalf("unknown model in scope: %v", err)
	}
	_ = root
}

func TestLayoutProfileModelOutsideScopeIsAnError(t *testing.T) {
	layout, location, _ := sampleLayout(t)
	editJSON(t, filepath.Join(location.Directory, "session.json"), func(m map[string]any) {
		m["scope"].(map[string]any)["models"] = []any{"category:openai"}
	})
	// reviewer lists haiku, which is in category cheap: outside the session scope
	_, err := LoadLayoutSessionSettings(location, layout, "reviewer", "")
	if err == nil || !strings.Contains(err.Error(), "haiku") {
		t.Fatalf("expected an error naming haiku, got %v", err)
	}
}

func TestLayoutAmbiguousMCPNameNeedsACategory(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "tools", "coding", "full.json"), func(m map[string]any) {
		mcp := m["mcp"].(map[string]any)
		delete(mcp, "browser-headed/betterwright")
		mcp["betterwright"] = map[string]any{}
	})
	_, err := LoadLayoutSessionSettings(location, layout, "coder", "")
	if err == nil || !strings.Contains(err.Error(), "betterwright") || !strings.Contains(err.Error(), "browser-headed") ||
		!strings.Contains(err.Error(), "browser-headless") {
		t.Fatalf("ambiguous name should list both files: %v", err)
	}
}

func TestLayoutRefusesSettingsThatHaveTheirOwnPlace(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) {
		m["settings"].(map[string]any)["cwd"] = "/tmp"
	})
	if _, err := LoadLayoutSessionSettings(location, layout, "coder", ""); err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("cwd in profile settings must be refused: %v", err)
	}
}

func TestLayoutRefusesAnUnknownKeyInACatalogFile(t *testing.T) {
	layout, location, root := sampleLayout(t)
	editJSON(t, filepath.Join(root, "profiles", "dev", "coder.json"), func(m map[string]any) { m["toolsett"] = "full" })
	if _, err := LoadLayoutSessionSettings(location, layout, "coder", ""); err == nil || !strings.Contains(err.Error(), "toolsett") {
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
	config, err := LoadLayoutSessionSettings(location, layout, "reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxToolRounds != 20 {
		t.Fatalf("session should beat defaults: %d", config.MaxToolRounds)
	}
	editJSON(t, filepath.Join(root, "profiles", "review", "reviewer.json"), func(m map[string]any) {
		m["settings"] = map[string]any{"max_tool_rounds": 30}
	})
	config, err = LoadLayoutSessionSettings(location, layout, "reviewer", "")
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
