package native

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// lateLazyCatalog builds the catalog of one late-built native tool, lazy under set, with hooks.
func lateLazyCatalog(t *testing.T, name string, set *tools.LazySet, hooks sessionfs.HooksSettings) *tools.Catalog {
	t.Helper()
	config := sessionfs.SessionConfig{
		SettingPath:  filepath.Join(t.TempDir(), "setting.json"),
		Profile:      "main",
		Profiles:     []string{"main", "reviewer"},
		Hooks:        hooks,
		StoragePaths: sessionfs.SessionStoragePaths{Messages: filepath.Join(t.TempDir(), "messages.jsonl")},
		ProfileTools: sessionfs.ProfileToolSettings{Lazy: []string{name}},
	}
	manifest := map[string]string{
		"transcript":      "transcript.json",
		"transcripts":     "transcripts.json",
		"profile":         "profile.json",
		"profile_message": "profile_message.json",
		"subagent":        "subagent.json",
	}[name]
	config.ToolDefinitionPaths = []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", manifest)}
	if name == "subagent" {
		config.ToolDefinitionPaths = append(config.ToolDefinitionPaths, filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"))
	}
	switch name {
	case "transcript":
		config.NativeTranscript = &sessionfs.NativeTranscriptSettings{}
	case "transcripts":
		config.NativeTranscripts = &sessionfs.NativeTranscriptsSettings{}
	case "profile":
		config.NativeProfile = &sessionfs.NativeProfileSettings{}
	case "profile_message":
		config.NativeProfileMessage = &sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024}
	case "subagent":
		reader := agentNamed("explore")
		reader.Tools, reader.HasTools = []string{"read"}, true
		config.NativeRead = &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100}
		config.NativeSubagent = &sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{reader}}
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, &fakeLauncher{}, nil, nil, nil, messenger, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync(%s): %v", name, err)
	}
	return catalog
}

// Each late-built native tool, and subagent, can be lazy: a call before its load is refused with
// tool_not_loaded, and after the set loads it the call is not refused.
func TestLateNativeToolIsRefusedBeforeLoadAndRunsAfter(t *testing.T) {
	arguments := map[string]string{"transcript": `{}`, "transcripts": `{}`, "profile": `{"action":"info"}`, "profile_message": `{"profile":"reviewer","message":"hi"}`, "subagent": `{"task":"x","agent":"explore"}`}
	for name, args := range arguments {
		t.Run(name, func(t *testing.T) {
			set, err := tools.OpenLazySet(t.TempDir(), []string{name}, 256, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			catalog := lateLazyCatalog(t, name, set, sessionfs.HooksSettings{})
			if before := executeNamedCall(t, catalog, name, args); !strings.Contains(before, "tool_not_loaded") {
				t.Fatalf("before load: want tool_not_loaded, got %s", before)
			}
			if err := set.Load(name); err != nil {
				t.Fatal(err)
			}
			if after := executeNamedCall(t, catalog, name, args); strings.Contains(after, "tool_not_loaded") {
				t.Fatalf("after load: still refused: %s", after)
			}
		})
	}
}

// A late tool's pre_tool hook fires once per call: once for a refused call before load, and once more for
// a loaded call after it. The gate sits under the hook, so a refusal is one hook firing and a loaded call
// is not wrapped twice. TestLoadedLazyNativeCallFiresItsHookOnce is the app-level version of this check.
// subagent is checked too, because its parent copy is hooked and gated like the late tools.
func TestLateNativeToolHookFiresOncePerCallBeforeAndAfterLoad(t *testing.T) {
	calls := map[string]string{"transcript": `{}`, "transcripts": `{}`, "profile": `{"action":"info"}`, "profile_message": `{"profile":"reviewer","message":"hi"}`, "subagent": `{"task":"x","agent":"explore"}`}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "fires")
			pre := eventHook("log", "*", "echo fired >> "+marker)
			set, err := tools.OpenLazySet(t.TempDir(), []string{name}, 256, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			catalog := lateLazyCatalog(t, name, set, eventSettings([]sessionfs.ToolEventHook{pre}, nil))
			executeNamedCall(t, catalog, name, args)
			if got := countLines(t, marker); got != 1 {
				t.Fatalf("pre_tool fired %d times for one refused call, want 1", got)
			}
			if err := set.Load(name); err != nil {
				t.Fatal(err)
			}
			executeNamedCall(t, catalog, name, args)
			if got := countLines(t, marker); got != 2 {
				t.Fatalf("pre_tool fired %d times after one refused and one loaded call, want 2", got)
			}
		})
	}
}

// A child of a parent whose lazy list names profile_message gets the owner's gated copy through childPool:
// the child's call before the owner's set loads it is refused as tool_not_loaded, and the target never runs.
func TestChildOfAParentWithALazyProfileMessageGetsTheGatedCopy(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"profile_message"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	childAgent := agentNamed("explore")
	childAgent.Tools, childAgent.HasTools = []string{"profile_message"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:          filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:  []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")},
		NativeSubagent:       &subagent,
		NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024},
		Profile:              "main",
		Profiles:             []string{"main", "reviewer"},
	}
	launcher := &fakeLauncher{}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, launcher, nil, nil, nil, messenger, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	childCall := executeNamedCall(t, launcher.requests[0].Tools, "profile_message", `{"profile":"reviewer","message":"hi"}`)
	if !strings.Contains(childCall, "tool_not_loaded") {
		t.Fatalf("the child's call to an unloaded lazy profile_message was not refused: %s", childCall)
	}
	if messenger.owner != "" {
		t.Fatalf("the refused child call reached the messenger for %q", messenger.target)
	}
}

// A nested subagent is the owner's subagent tool given to a child, one level down. The child's own catalog
// holds a lazy copy of subagent and a lazy copy of read: both are refused until the owner's set loads them,
// as the resolver wires it: the catalog is built, then SetLazy gives the set to the subagent tool.
func TestNestedChildGatesLazySubagentAndReadUntilLoaded(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"subagent", "read"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	childAgent := nestingAgent("explore", "read", "subagent")
	settings := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeRead:          &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeSubagent:      &settings,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Lazy: []string{"subagent", "read"}},
	}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	_, err = BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, &fakeLauncher{}, mcp, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	if err := mcp.SetLazy(set, nil); err != nil {
		t.Fatal(err)
	}
	if len(mcp.tool.agents) != 1 || mcp.tool.agents[0].catalog == nil {
		t.Fatal("the child catalog was not built")
	}
	child := mcp.tool.agents[0].catalog
	for _, name := range []string{"read", "subagent"} {
		args := `{"path":"notes.txt"}`
		if name == "subagent" {
			args = `{"task":"y","agent":"explore"}`
		}
		if before := executeNamedCall(t, child, name, args); !strings.Contains(before, "tool_not_loaded") {
			t.Fatalf("the nested child's call to unloaded %s was not refused: %s", name, before)
		}
	}
	for _, name := range []string{"read", "subagent"} {
		if err := set.Load(name); err != nil {
			t.Fatal(err)
		}
	}
	if read := executeNamedCall(t, child, "read", `{"path":"notes.txt"}`); strings.Contains(read, "tool_not_loaded") {
		t.Fatalf("the nested child's read after load was refused: %s", read)
	}
	if nested := executeNamedCall(t, child, "subagent", `{"task":"y","agent":"explore"}`); strings.Contains(nested, "tool_not_loaded") {
		t.Fatalf("the nested child's subagent after load was refused: %s", nested)
	}
}
