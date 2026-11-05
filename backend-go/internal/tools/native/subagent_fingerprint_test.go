package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// fingerprintOf starts one run of agent with the given MCP tools bound and returns the
// fingerprint its manifest holds. The manifest is read from disk, as a restart would.
func fingerprintOf(t *testing.T, root string, agent sessionfs.NativeSubagentAgent, settings sessionfs.NativeSubagentSettings, mcpTools []tools.Tool) string {
	t.Helper()
	settings.StorageDir = root
	settings.PersistRuns = true
	host := newTestAsyncHost(t)
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	parent := withExecutors(t, readOnlyTool("read"))
	hook := &SubagentMCP{}
	tool, err := newSubagentToolAsync(settings, parent, nil, &fakeLauncher{}, hook, manager, host)
	if err != nil {
		t.Fatalf("newSubagentToolAsync: %v", err)
	}
	if err := hook.Bind(mcpTools); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	catalog := catalogWithAsync(t, host, parent, tool)
	id := startedRunID(t, catalog, manager, testSessionKey, `{"task":"look"}`)
	return manifestOf(t, root, id).Config
}

func TestConfigFingerprintIsAHashAndNeverTheRawConfig(t *testing.T) {
	agent := agentWithTools("explore", "read")
	agent.PromptText = "SECRET-PROMPT-TEXT"
	root := t.TempDir()
	got := fingerprintOf(t, root, agent, subagentSettings(agent), nil)
	if len(got) != 64 || strings.Contains(got, "SECRET") {
		t.Fatalf("fingerprint = %q, want a 64-hex hash", got)
	}
	files, err := filepath.Glob(filepath.Join(root, subagentManifestDir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("manifests = %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET-PROMPT-TEXT") {
		t.Fatalf("the manifest holds the raw prompt:\n%s", data)
	}
}

func TestConfigNoteNeedsBothFingerprintsAndADifference(t *testing.T) {
	if got := configChangedNote("", "abc"); got != "" {
		t.Fatalf("missing source fingerprint produced a note: %q", got)
	}
	if got := configChangedNote("abc", ""); got != "" {
		t.Fatalf("missing current fingerprint produced a note: %q", got)
	}
	if got := configChangedNote("abc", "abc"); got != "" {
		t.Fatalf("matching fingerprints produced a note: %q", got)
	}
	if got := configChangedNote("abc", "def"); !strings.Contains(got, "agent definition changed") {
		t.Fatalf("changed fingerprints gave no note: %q", got)
	}
}

func TestV1ManifestWithoutAConfigFieldLoadsWithNoNote(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	manifest := subagentManifest{
		Version: 1, ID: "sub_0000abcd", Agent: "explore", Task: "x", State: subagentRunDone,
		StartedAt: now, EndedAt: now,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"config"`) {
		t.Fatalf("an empty fingerprint is written: %s", data)
	}
	if err := os.MkdirAll(filepath.Join(root, subagentManifestDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, subagentManifestDir, "sub_0000abcd.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: root, Keep: 10})
	t.Cleanup(func() { _ = manager.Close() })
	run := manager.byID("sub_0000abcd")
	if run == nil || run.config != "" {
		t.Fatalf("v1 run = %+v, want loaded with no fingerprint", run)
	}
	if got := configChangedNote(run.config, "anything"); got != "" {
		t.Fatalf("a v1 run produced a note: %q", got)
	}
}

func TestChangedMCPDefinitionBetweenTwoStartsGivesTheFingerprintChange(t *testing.T) {
	agent := agentWithTools("explore", "read", "mcp:srv:search")
	settings := subagentSettings(agent)
	before := fingerprintOf(t, t.TempDir(), agent, settings, []tools.Tool{mcpStub(t, "srv", "search")})
	changed := mcpStub(t, "srv", "search")
	changed.Definition.Description = "search, with a different description"
	after := fingerprintOf(t, t.TempDir(), agent, settings, []tools.Tool{changed})
	if before == after {
		t.Fatal("a changed MCP definition did not change the fingerprint")
	}
	if configChangedNote(before, after) == "" {
		t.Fatal("a changed MCP definition gave no config note")
	}
}

func TestInheritedSettingChangeGivesNoNote(t *testing.T) {
	// The compared scope is the agent definition. The session's inherited settings are not in
	// it, so changing one gives the same fingerprint and no note.
	agent := agentWithTools("explore", "read")
	before := fingerprintOf(t, t.TempDir(), agent, subagentSettings(agent), nil)
	inherited := subagentSettings(agent)
	inherited.MaxQueued = 99
	inherited.ResultMaxBytes = 4096
	after := fingerprintOf(t, t.TempDir(), agent, inherited, nil)
	if before != after {
		t.Fatalf("an inherited setting changed the fingerprint: %q and %q", before, after)
	}
	if configChangedNote(before, after) != "" {
		t.Fatal("an inherited-setting change gave a config note")
	}
}
