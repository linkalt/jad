package native

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// lateLazyCatalog builds the catalog of one late-built native tool, with the lazy set handed to it (its entry names the tool, which a lazy set must never bind), and hooks.
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
	config.ToolDefinitionPaths = []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", manifest), sampleLoadToolManifest}
	config.NativeLoadTool = &sessionfs.NativeLoadToolSettings{}
	if name == "subagent" {
		config.ToolDefinitionPaths = append(config.ToolDefinitionPaths, filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"))
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
		reader.Tools, reader.HasTools = []string{"read_image"}, true
		config.NativeReadImage = &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20}
		config.NativeSubagent = &sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{reader}}
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, &fakeLauncher{}, nil, nil, nil, messenger, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync(%s): %v", name, err)
	}
	return catalog
}

// A native tool is never lazy, the late-built ones (transcript, transcripts, profile, profile_message) and subagent
// included, even when the set is handed an entry that names one: it is listed, a direct call runs it, and
// load_tool run answers that it is not a lazy tool of this profile.
func TestLateNativeToolIsNeverLazy(t *testing.T) {
	arguments := map[string]string{"transcript": `{}`, "transcripts": `{}`, "profile": `{"action":"info"}`, "profile_message": `{"profile":"reviewer","message":"hi"}`, "subagent": `{"task":"x","agent":"explore"}`}
	for name, args := range arguments {
		t.Run(name, func(t *testing.T) {
			set := tools.NewLazySet([]string{name}, 256, nil)
			catalog := lateLazyCatalog(t, name, set, sessionfs.HooksSettings{})
			listed := false
			for _, definition := range catalog.ListedDefinitions() {
				listed = listed || definition.Name == name
			}
			if !listed {
				t.Fatalf("%s is not listed", name)
			}
			if len(set.Entries()) != 0 {
				t.Fatalf("the set bound %v for the native tool %s", set.Entries(), name)
			}
			if direct := executeNamedCall(t, catalog, name, args); strings.Contains(direct, "lazy_tool_not_loaded") || strings.Contains(direct, "is lazy") {
				t.Fatalf("a direct call of the native tool %s was refused as lazy: %s", name, direct)
			}
			if via := executeThroughLoadTool(t, catalog, name, args); !strings.Contains(via, "is not a lazy tool of this profile") {
				t.Fatalf("load_tool run of the native tool %s: want %q, got %s", name, "is not a lazy tool of this profile", via)
			}
		})
	}
}

// A late tool's pre_tool hook fires once per direct call, and a load_tool run of a native tool is refused by the
// runner before any dispatch, so it fires no hook of the target's.
func TestLateNativeToolHookFiresOncePerDirectCall(t *testing.T) {
	calls := map[string]string{"transcript": `{}`, "transcripts": `{}`, "profile": `{"action":"info"}`, "profile_message": `{"profile":"reviewer","message":"hi"}`, "subagent": `{"task":"x","agent":"explore"}`}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "fires")
			pre := eventHook("log", "*", "echo fired >> "+marker)
			set := tools.NewLazySet([]string{name}, 256, nil)
			catalog := lateLazyCatalog(t, name, set, eventSettings([]sessionfs.ToolEventHook{pre}, nil))
			executeNamedCall(t, catalog, name, args)
			if got := countLines(t, marker); got != 1 {
				t.Fatalf("pre_tool fired %d times for one direct call, want 1", got)
			}
			executeThroughLoadTool(t, catalog, name, args)
			if got := countLines(t, marker); got != 1 {
				t.Fatalf("pre_tool fired %d times after a direct call and a refused load_tool run, want still 1", got)
			}
		})
	}
}

// A child of a parent whose lazy set is handed profile_message gets the ordinary copy through childPool: the
// child's call runs and reaches the messenger.
func TestChildOfAParentNamingProfileMessageLazyStillGetsItEager(t *testing.T) {
	set := tools.NewLazySet([]string{"profile_message"}, 256, nil)
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
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, nil, nil, nil, messenger, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	childCall := executeNamedCall(t, launcher.requests[0].Tools, "profile_message", `{"profile":"reviewer","message":"hi"}`)
	if strings.Contains(childCall, "lazy_tool_not_loaded") {
		t.Fatalf("the child's call to profile_message was refused as lazy: %s", childCall)
	}
	if messenger.target != "reviewer" {
		t.Fatalf("the child's call did not reach the messenger (target %q)", messenger.target)
	}
}

// A nested subagent is the owner's subagent tool given to a child, one level down. Neither the child's copy of
// subagent nor of read_image is lazy, as the resolver wires it (the catalog is built, then SetLazy gives the
// set to the subagent tool): both run when named directly.
func TestNestedChildHoldsNativeSubagentAndReadEager(t *testing.T) {
	set := tools.NewLazySet([]string{"subagent", "read_image"}, 256, nil)
	childAgent := nestingAgent("explore", "read_image", "subagent")
	settings := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:      &settings,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Lazy: []string{"subagent", "read_image"}},
	}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	launcher := &fakeLauncher{}
	_, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, mcp, nil, nil, nil, nil, set)
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
	for _, name := range []string{"read_image", "subagent"} {
		args := `{"path":"notes.png"}`
		if name == "subagent" {
			args = `{"task":"y","agent":"explore"}`
		}
		if got := executeNamedCall(t, child, name, args); strings.Contains(got, "lazy_tool_not_loaded") {
			t.Fatalf("the nested child's direct call to %s was refused as lazy: %s", name, got)
		}
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("the nested subagent call started %d children, want 1", len(launcher.requests))
	}
}
