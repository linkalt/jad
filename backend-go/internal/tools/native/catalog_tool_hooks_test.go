package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// hookedCatalogSettings builds a catalog that lists read, subagent and profile_message, with a
// pre_tool hook on every tool. The hook's marker is one line per call; a blocking hook is
// added when block is set.
func hookedCatalogFixture(t *testing.T, launcher SubagentLauncher, marker string, block bool) *tools.Catalog {
	t.Helper()
	pre := eventHook("log", "*", "echo fired >> "+marker)
	if block {
		pre = blockingHook("gate", "subagent", `echo no-subagents; exit 3`)
	}
	readerAgent := agentNamed("explore")
	readerAgent.Tools, readerAgent.HasTools = []string{"read"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{readerAgent}}
	settings := sessionfs.SessionConfig{
		SettingPath:          filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:  []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")},
		NativeRead:           &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeSubagent:       &subagent,
		NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024},
		Profile:              "main",
		Profiles:             []string{"main", "reviewer"},
		Hooks:                eventSettings([]sessionfs.ToolEventHook{pre}, nil),
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithMessenger(settings, nil, nil, nil, nil, nil, launcher, nil, nil, nil, messenger)
	if err != nil {
		t.Fatalf("BuildCatalogWithMessenger: %v", err)
	}
	return catalog
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func executeNamedCall(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The pre_tool hook fires for the subagent call and for the profile_message call; each one
// reports to the marker once.
func TestPreToolHookFiresForSubagentAndProfileMessageCalls(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fires")
	launcher := &fakeLauncher{}
	catalog := hookedCatalogFixture(t, launcher, marker, false)
	executeNamedCall(t, catalog, "subagent", `{"task":"x"}`)
	executeNamedCall(t, catalog, "profile_message", `{"profile":"reviewer","message":"hi"}`)
	if got := countLines(t, marker); got != 2 {
		t.Fatalf("pre_tool fired %d times for a subagent and a profile_message call, want 2", got)
	}
}

// A blocking pre_tool on subagent stops the run: the launcher never starts the child.
func TestBlockingPreToolStopsASubagentRun(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog := hookedCatalogFixture(t, launcher, filepath.Join(t.TempDir(), "fires"), true)
	output := executeNamedCall(t, catalog, "subagent", `{"task":"x"}`)
	if len(launcher.requests) != 0 {
		t.Fatalf("the child ran %d times although a blocking pre_tool hook failed", len(launcher.requests))
	}
	if !strings.Contains(output, "blocked_by_hook") || !strings.Contains(output, "no-subagents") {
		t.Fatalf("output = %s, want the blocked result with the hook's report", output)
	}
}

// A native lazy tool is gated for a child too: the child's copy of read is the gated one, so a child
// calling it before a load is refused with tool_not_loaded, and the real schema is not run.
func TestChildOfAParentWithANativeLazyToolGetsTheGatedCopy(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	readerAgent := agentNamed("explore")
	readerAgent.Tools, readerAgent.HasTools = []string{"read"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{readerAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeRead:          &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeSubagent:      &subagent,
		Profile:             "main",
	}
	launcher := &fakeLauncher{}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, launcher, nil, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	childRead := executeNamedCall(t, launcher.requests[0].Tools, "read", `{"path":"notes.txt"}`)
	if !strings.Contains(childRead, "tool_not_loaded") {
		t.Fatalf("the child's read of an unloaded lazy tool was not refused: %s", childRead)
	}
}
