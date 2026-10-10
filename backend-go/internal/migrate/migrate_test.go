package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/sessionlock"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeObject(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

// legacyTree builds a root setting.json and two sessions: "alpha" with two profiles that
// have their own state, and "beta" with one profile and a different betterwright config.
func legacyTree(t *testing.T) (setting string, cwd string) {
	t.Helper()
	root := t.TempDir()
	cwd = t.TempDir()
	writeObject(t, filepath.Join(root, "setting.json"), map[string]any{
		"schema_version": 1,
		"server":         map[string]any{"port": 9000},
		"session_roots":  []any{map[string]any{"id": "local", "path": "sessions"}},
	})
	writeObject(t, filepath.Join(root, "mcp", "betterwright.json"), map[string]any{"transport": map[string]any{"type": "streamable_http", "url": "http://127.0.0.1:1/headed"}})
	writeObject(t, filepath.Join(root, "mcp", "betterwright-headless.json"), map[string]any{"transport": map[string]any{"type": "streamable_http", "url": "http://127.0.0.1:1/headless"}})

	model := func(name string) map[string]any {
		return map[string]any{"provider": "openai", "model": name, "api_key_env": "K", "base_url": "http://127.0.0.1:1/v1"}
	}
	alpha := filepath.Join(root, "sessions", "alpha")
	writeObject(t, filepath.Join(alpha, "model-a.json"), model("model-a"))
	writeObject(t, filepath.Join(alpha, "model-b.json"), model("model-b"))
	writeObject(t, filepath.Join(alpha, "setting.json"), map[string]any{
		"cwd":              cwd,
		"session_name":     "Alpha work",
		"max_tool_rounds":  7,
		"prompt_documents": []string{},
		"tools":            map[string]any{"native": map[string]any{"read": map[string]any{}, "grep": map[string]any{}}},
		"mcp_servers": []any{
			map[string]any{"id": "betterwright", "enabled": true, "config_path": filepath.Join(root, "mcp", "betterwright.json"), "deny_tools": []string{"x"}},
			map[string]any{"id": "off-server", "enabled": false, "config_path": filepath.Join(root, "mcp", "betterwright.json")},
		},
		"paths": map[string]any{
			"active_profile": "second",
			"profiles": map[string]any{
				"first":  map[string]any{"model_default": "model-a.json", "models": []string{"model-a.json", "model-b.json"}, "messages": "first/messages.jsonl", "staging": "first/staging"},
				"second": map[string]any{"model_default": "model-b.json", "models": []string{"model-b.json"}, "messages": "second/messages.jsonl", "staging": "second/staging", "tool_storage": "second-tools"},
			},
		},
	})
	writeFile(t, filepath.Join(alpha, "first", "messages.jsonl"), "{\"first\":1}\n")
	writeFile(t, filepath.Join(alpha, "first", "staging", "usage-cache.json"), "{\"u\":1}")
	writeFile(t, filepath.Join(alpha, "second", "messages.jsonl"), "{\"second\":2}\n")
	writeFile(t, filepath.Join(cwd, "second-tools", ".jad", "task", "t.json"), "{}")

	beta := filepath.Join(root, "sessions", "beta")
	writeObject(t, filepath.Join(beta, "model.json"), model("model-c"))
	writeObject(t, filepath.Join(beta, "setting.json"), map[string]any{
		"cwd": cwd,
		"mcp_servers": []any{
			map[string]any{"id": "betterwright", "enabled": true, "config_path": filepath.Join(root, "mcp", "betterwright-headless.json"), "deny_tools": []string{}},
		},
		"paths": map[string]any{
			"active_profile": "only",
			"profiles":       map[string]any{"only": map[string]any{"model_default": "model.json", "messages": "only/messages.jsonl"}},
		},
	})
	writeFile(t, filepath.Join(beta, "only", "messages.jsonl"), "{\"beta\":3}\n")
	return filepath.Join(root, "setting.json"), cwd
}

// treeHash is the hash of every file name and content under a directory.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	var names []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		// A lock file is what the transcript lock leaves behind; a server run leaves the same.
		if err == nil && info.Mode().IsRegular() && !sessionlock.IsLockFileName(info.Name()) {
			names = append(names, path)
		}
		return nil
	})
	sort.Strings(names)
	sum := sha256.New()
	for _, name := range names {
		data, _ := os.ReadFile(name)
		sum.Write([]byte(name))
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func TestMigrateConvertsEverySessionAndChangesNothingInTheSource(t *testing.T) {
	setting, _ := legacyTree(t)
	before := treeHash(t, filepath.Dir(setting))
	out := filepath.Join(t.TempDir(), "new")

	report, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Differences) != 0 {
		t.Fatalf("migrated tree differs: %v", report.Differences)
	}
	if len(report.Sessions) != 2 {
		t.Fatalf("sessions = %+v", report.Sessions)
	}
	if after := treeHash(t, filepath.Dir(setting)); after != before {
		t.Fatal("the source tree was changed")
	}

	// Each profile has its own folder with its own transcript.
	for path, want := range map[string]string{
		"state/alpha/alpha/first/messages.jsonl":           "{\"first\":1}\n",
		"state/alpha/alpha/second/messages.jsonl":          "{\"second\":2}\n",
		"state/alpha/alpha/first/staging/usage-cache.json": "{\"u\":1}",
		"state/beta/beta/only/messages.jsonl":              "{\"beta\":3}\n",
		"state/alpha/alpha/second/tools/.jad/task/t.json":  "{}",
	} {
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(path)))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", path, data, err, want)
		}
	}

	// The migrated tree is a server's input: the root loads, and the session runs as a layout.
	config, err := sessionfs.Load(sessionfs.LoadOptions{RootSettingPath: filepath.Join(out, "setting.json"), Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(config.Roots, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !location.UsesLayout() {
		t.Fatal("the migrated session is not a layout session")
	}
	loaded, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Profile != "alpha/second" || loaded.ModelDefault != "alpha/model-b" || loaded.SessionName != "Alpha work" || loaded.MaxToolRounds != 7 {
		t.Fatalf("profile %q, model %q, name %q, rounds %d", loaded.Profile, loaded.ModelDefault, loaded.SessionName, loaded.MaxToolRounds)
	}
	if strings.Join(loaded.PathProfiles, ",") != "alpha/first,alpha/second" {
		t.Fatalf("profiles = %v", loaded.PathProfiles)
	}
}

// Two legacy sessions with one server id and different config files must not collapse into
// one file: each keeps its own environment, in its own category.
func TestMigrateKeepsTwoEnvironmentsOfOneServerApart(t *testing.T) {
	setting, _ := legacyTree(t)
	out := filepath.Join(t.TempDir(), "new")
	if _, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	var found []string
	_ = filepath.Walk(filepath.Join(out, "mcp"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() == "betterwright.json" {
			rel, _ := filepath.Rel(filepath.Join(out, "mcp"), path)
			found = append(found, rel)
		}
		return nil
	})
	sort.Strings(found)
	if len(found) != 2 || filepath.Dir(found[0]) == filepath.Dir(found[1]) {
		t.Fatalf("mcp files = %v, want one betterwright.json in each of two categories", found)
	}
	first, _ := os.ReadFile(filepath.Join(out, "mcp", found[0]))
	second, _ := os.ReadFile(filepath.Join(out, "mcp", found[1]))
	both := string(first) + string(second)
	if strings.Count(both, "/headed") != 1 || strings.Count(both, "/headless") != 1 {
		t.Fatalf("the two environments were merged or lost: %s / %s", first, second)
	}
	// alpha uses the headed file and beta the headless one; the toolsets must point at them.
	for session, want := range map[string]string{"alpha": "/headed", "beta": "/headless"} {
		toolset, _ := os.ReadFile(filepath.Join(out, "tools", session, "main.json"))
		var parsed struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if err := json.Unmarshal(toolset, &parsed); err != nil {
			t.Fatal(err)
		}
		var used string
		for key := range parsed.MCP {
			if strings.HasSuffix(key, "/betterwright") {
				used = key
			}
		}
		data, _ := os.ReadFile(filepath.Join(out, "mcp", filepath.FromSlash(used)+".json"))
		if used == "" || !strings.Contains(string(data), want) {
			t.Fatalf("%s's toolset uses %q = %s, want the %s file", session, used, data, want)
		}
	}
	// A server the session lists as disabled stays listed and disabled.
	toolset, _ := os.ReadFile(filepath.Join(out, "tools", "alpha", "main.json"))
	if !strings.Contains(string(toolset), `"enabled": false`) {
		t.Fatalf("a disabled server became enabled: %s", toolset)
	}
}

func TestMigrateRefusesANonEmptyOutputAndAnActiveRun(t *testing.T) {
	setting, _ := legacyTree(t)
	out := t.TempDir()
	writeFile(t, filepath.Join(out, "keep.txt"), "x")
	if _, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty output error = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "keep.txt")); string(data) != "x" {
		t.Fatal("the output directory was touched")
	}

	// A run holds the transcript lock: the profile is in use.
	messages := filepath.Join(filepath.Dir(setting), "sessions", "alpha", "second", "messages.jsonl")
	lockPath, _ := sessionlock.PathFor(messages)
	held, err := sessionlock.Acquire(t.Context(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = Run(Options{SourceSetting: setting, Out: filepath.Join(t.TempDir(), "new"), Environment: map[string]string{}, LockWait: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("active run error = %v, want it refused as in use", err)
	}
}

// A session that is already in the layout is left alone.
func TestMigrateSkipsASessionThatAlreadyHasASessionFile(t *testing.T) {
	setting, _ := legacyTree(t)
	writeObject(t, filepath.Join(filepath.Dir(setting), "sessions", "beta", "session.json"), map[string]any{"schema_version": 1})
	out := filepath.Join(t.TempDir(), "new")
	report, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "beta") {
		t.Fatalf("sessions = %+v, skipped = %v", report.Sessions, report.Skipped)
	}
}

func TestVerifyReportsAChangedSetting(t *testing.T) {
	setting, _ := legacyTree(t)
	out := filepath.Join(t.TempDir(), "new")
	if _, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	legacy, migrated := sessionfs.SessionConfig{MaxToolRounds: 7}, sessionfs.SessionConfig{MaxToolRounds: 8}
	if got := compare(legacy, migrated, func(s string) string { return s }); len(got) != 1 || !strings.Contains(got[0], "MaxToolRounds") {
		t.Fatalf("compare = %v", got)
	}
	if got := compare(legacy, legacy, func(s string) string { return s }); len(got) != 0 {
		t.Fatalf("equal configs differ: %v", got)
	}
}

// A session without paths.profiles keeps its tool state under the cwd; the layout cannot
// carry that over, so the session is skipped with the reason and nothing is half-moved.
func TestMigrateSkipsASessionWithoutProfilesAndSaysWhy(t *testing.T) {
	setting, cwd := legacyTree(t)
	writeObject(t, filepath.Join(filepath.Dir(setting), "sessions", "gamma", "setting.json"), map[string]any{"cwd": cwd})
	out := filepath.Join(t.TempDir(), "new")
	report, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 2 || len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0], "gamma") || !strings.Contains(report.Skipped[0], "paths.profiles") {
		t.Fatalf("sessions = %d, skipped = %v", len(report.Sessions), report.Skipped)
	}
	if _, err := os.Stat(filepath.Join(out, "sessions", "gamma")); err == nil {
		t.Fatal("a skipped session left files behind")
	}
}

// A sub-agent run manifest names its transcript folder by absolute path and its owner as
// "<session>\x00<profile>". Both must follow the profile to its new place.
func TestMigrateRewritesSubagentRunManifests(t *testing.T) {
	setting, cwd := legacyTree(t)
	tools := filepath.Join(cwd, "second-tools")
	transcript := filepath.Join(tools, ".subagents", "run-folder")
	writeFile(t, filepath.Join(transcript, "messages.jsonl"), "{}\n")
	writeObject(t, filepath.Join(tools, ".subagents", ".runs", "sub_1.json"), map[string]any{
		"version": 1, "id": "sub_1", "owner": "alpha\x00second", "state": "done", "dir": transcript,
	})
	out := filepath.Join(t.TempDir(), "new")
	report, err := Run(Options{SourceSetting: setting, Out: out, Environment: map[string]string{}})
	if err != nil || len(report.Differences) != 0 {
		t.Fatalf("err = %v, differences = %v", err, report.Differences)
	}
	newTools := filepath.Join(out, "state", "alpha", "alpha", "second", "tools")
	data, err := os.ReadFile(filepath.Join(newTools, ".subagents", ".runs", "sub_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Owner, Dir string }
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Owner != "alpha\x00alpha/second" || manifest.Dir != filepath.Join(newTools, ".subagents", "run-folder") {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(manifest.Dir, "messages.jsonl")); err != nil {
		t.Fatalf("the folder the manifest names is missing: %v", err)
	}
	// the source manifest is untouched
	orig, _ := os.ReadFile(filepath.Join(tools, ".subagents", ".runs", "sub_1.json"))
	if !strings.Contains(string(orig), "alpha\\u0000second") {
		t.Fatalf("the source manifest changed: %s", orig)
	}
}
