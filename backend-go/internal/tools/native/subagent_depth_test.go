package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A child of a child: the owner's lazy set gates the copies at every level. One set serves all three levels,
// so a tool the owner has not loaded is refused wherever it is listed. The parent refuses subagent until the
// owner loads it; the child refuses its lazy read until the owner loads read; the grandchild, started by the
// child's nested subagent, refuses its read the same way.
func TestGrandchildGatesALazyReadUntilTheOwnerLoadsIt(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read", "subagent"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	childAgent := nestingAgent("explore", "read", "subagent")
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 3, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeRead:          &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeSubagent:      &subagent,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Lazy: []string{"read", "subagent"}},
	}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	launcher := &fakeLauncher{}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, mcp, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	if err := mcp.SetLazy(set, nil); err != nil {
		t.Fatal(err)
	}
	if out := subagentCall(t, catalog, `{"task":"x","agent":"explore"}`); !strings.Contains(out, "not loaded") || len(launcher.requests) != 0 {
		t.Fatalf("the parent's call to the unloaded lazy subagent was not refused: %s", out)
	}
	if err := set.Load("subagent"); err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("the child ran %d times, want 1", len(launcher.requests))
	}
	child := launcher.requests[0].Tools
	if out := executeNamedCall(t, child, "read", `{"path":"notes.txt"}`); !strings.Contains(out, "not loaded") {
		t.Fatalf("the child's read before load was not refused: %s", out)
	}
	// The grandchild is started while read is still unloaded: the read copy it holds is refused.
	subagentCall(t, child, `{"task":"y","agent":"explore"}`)
	if len(launcher.requests) != 2 {
		t.Fatalf("the grandchild ran %d times in total, want 2", len(launcher.requests))
	}
	if out := executeNamedCall(t, launcher.requests[1].Tools, "read", `{"path":"notes.txt"}`); !strings.Contains(out, "not loaded") {
		t.Fatalf("the grandchild's read before load was not refused: %s", out)
	}
	if err := set.Load("read"); err != nil {
		t.Fatal(err)
	}
	// A grandchild started after the owner loads read may read.
	subagentCall(t, child, `{"task":"z","agent":"explore"}`)
	if len(launcher.requests) != 3 {
		t.Fatalf("the second grandchild ran %d times in total, want 3", len(launcher.requests))
	}
	if out := executeNamedCall(t, launcher.requests[2].Tools, "read", `{"path":"notes.txt"}`); strings.Contains(out, "not loaded") {
		t.Fatalf("the grandchild's read after load was refused: %s", out)
	}
}

// The nested subagent copy a child holds is gated with the owner's set when it is built: its Refused is set
// while the owner has not loaded subagent, and the copy the child holds is the gated one. This pins the
// gate on the nested copy directly, because a call to it can only be made once the owner loads subagent.
func TestNestedSubagentCopyIsGatedBeforeTheOwnerLoadsIt(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"subagent"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	childAgent := nestingAgent("explore", "read", "subagent")
	settings := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	parent := withExecutors(t, readOnlyTool("read"))
	// The owner's subagent tool is built first, then bound, as BuildCatalogWithAsync does: a lazy copy needs a
	// real definition, validator and executor.
	owner, err := newSubagentToolAsync(settings, parent, nil, &fakeLauncher{}, mcp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Bind([]tools.Tool{owner})
	if err := mcp.SetLazy(set, nil); err != nil {
		t.Fatal(err)
	}
	for _, tool := range mcp.tool.agents[0].nativeTools {
		if tool.Definition.ID == subagentToolID && (tool.Refused == nil || !tool.Refused(tools.ExecutionContext{})) {
			t.Fatal("the nested subagent copy is not refused before the owner loads subagent")
		}
	}
}

// A grandchild's tool_smith shares the owner's bound list, so the owner's allow limits it as it limits a child's.
// The owner's allow hides read, and the grandchild lists read, so its script is refused read as not allowed. The
// grandchild is started by the child's nested subagent, one level below the owner's subagent tool.
func TestGrandchildToolSmithReachesNoMoreThanTheOwnersAllowList(t *testing.T) {
	root := shortTempDir(t)
	smith := bridgeSettings(t, root)
	smith.Tools = []string{"read"}
	childAgent := nestingAgent("explore", "read", "subagent", "tool_smith")
	launcher := &fakeLauncher{}
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeRead:          &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeToolSmith:     &smith,
		NativeSubagent:      &subagent,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"subagent", "tool_smith"}},
	}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, mcp, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("the child ran %d times, want 1", len(launcher.requests))
	}
	subagentCall(t, launcher.requests[0].Tools, `{"task":"y","agent":"explore"}`)
	if len(launcher.requests) != 2 {
		t.Fatalf("the grandchild ran %d times in total, want 2", len(launcher.requests))
	}
	got := runScriptThroughCatalog(t, launcher.requests[1].Tools, root, "read")
	if !strings.Contains(got, "read:invalid_input:tool read not allowed") {
		t.Fatalf("the grandchild's script reached read outside the owner's allow list: %s", got)
	}
}

// A tool_smith script at depth 3 is limited by the owner's allow list too. The owner's allow hides read, and the
// great-grandchild lists read, so its script is refused read as not allowed. The chain is owner, child, grandchild,
// great-grandchild, each started by the level above's nested subagent.
func TestDepthThreeToolSmithReachesNoMoreThanTheOwnersAllowList(t *testing.T) {
	root := shortTempDir(t)
	smith := bridgeSettings(t, root)
	smith.Tools = []string{"read"}
	childAgent := nestingAgent("explore", "read", "subagent", "tool_smith")
	launcher := &fakeLauncher{}
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 3, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeRead:          &sessionfs.NativeReadSettings{MaxBytes: 4096, DefaultLimit: 100},
		NativeToolSmith:     &smith,
		NativeSubagent:      &subagent,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"subagent", "tool_smith"}},
	}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, mcp, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("the child ran %d times, want 1", len(launcher.requests))
	}
	subagentCall(t, launcher.requests[0].Tools, `{"task":"y","agent":"explore"}`)
	if len(launcher.requests) != 2 {
		t.Fatalf("the grandchild ran %d times in total, want 2", len(launcher.requests))
	}
	subagentCall(t, launcher.requests[1].Tools, `{"task":"z","agent":"explore"}`)
	if len(launcher.requests) != 3 {
		t.Fatalf("the great-grandchild ran %d times in total, want 3", len(launcher.requests))
	}
	got := runScriptThroughCatalog(t, launcher.requests[2].Tools, root, "read")
	if !strings.Contains(got, "read:invalid_input:tool read not allowed") {
		t.Fatalf("the depth-3 script reached read outside the owner's allow list: %s", got)
	}
}
