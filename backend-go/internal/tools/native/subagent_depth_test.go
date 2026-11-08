package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A child of a child: a native tool is eager at every level, even when the owner's lazy set is handed an entry that
// names it (the loader refuses such an entry). The parent's subagent call starts the child, the child's read_image
// is not refused, and the grandchild started by the child's nested subagent holds an eager read_image too.
func TestNativeToolsAreEagerAtEveryNestingLevel(t *testing.T) {
	set := tools.NewLazySet([]string{"read_image", "subagent"}, 256, nil)
	childAgent := nestingAgent("explore", "read_image", "subagent")
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 3, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:      &subagent,
		Profile:             "main",
		Profiles:            []string{"main"},
		ProfileTools:        sessionfs.ProfileToolSettings{Lazy: []string{"read_image", "subagent"}},
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
	if out := subagentCall(t, catalog, `{"task":"x","agent":"explore"}`); strings.Contains(out, "lazy_tool_not_loaded") || len(launcher.requests) != 1 {
		t.Fatalf("the parent's direct subagent call was refused or did not start the child: %s", out)
	}
	child := launcher.requests[0].Tools
	if out := executeNamedCall(t, child, "read_image", `{"path":"notes.png"}`); strings.Contains(out, "lazy_tool_not_loaded") {
		t.Fatalf("the child's direct read_image was refused as lazy: %s", out)
	}
	executeNamedCall(t, child, "subagent", `{"task":"y","agent":"explore"}`)
	if len(launcher.requests) != 2 {
		t.Fatalf("the grandchild ran %d times in total, want 2", len(launcher.requests))
	}
	if out := executeNamedCall(t, launcher.requests[1].Tools, "read_image", `{"path":"notes.png"}`); strings.Contains(out, "lazy_tool_not_loaded") {
		t.Fatalf("the grandchild's direct read_image was refused as lazy: %s", out)
	}
}

// The nested subagent copy a child holds is never gated, even when the owner's set is handed an entry that
// names subagent: the set binds no native tool, so the copy has no Refused rule.
func TestNestedSubagentCopyIsNeverGated(t *testing.T) {
	set := tools.NewLazySet([]string{"subagent"}, 256, nil)
	childAgent := nestingAgent("explore", "read_image", "subagent")
	settings := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	mcp := NewSubagentMCP(sessionfs.HooksSettings{}, "")
	parent := withExecutors(t, readOnlyTool("read_image"))
	owner, err := newSubagentToolAsync(settings, parent, nil, &fakeLauncher{}, mcp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Bind([]tools.Tool{owner})
	if err := mcp.SetLazy(set, nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range mcp.tool.agents[0].nativeTools {
		if tool.Definition.ID != subagentToolID {
			continue
		}
		found = true
		if tool.Refused != nil {
			t.Fatal("the nested subagent copy has a Refused rule: a native tool is never lazy")
		}
	}
	if !found {
		t.Fatal("the child holds no nested subagent copy")
	}
}

// A grandchild's tool_smith shares the owner's bound list, so the owner's allow limits it as it limits a child's.
// The owner's allow hides read, and the grandchild lists read, so its script is refused read as not allowed. The
// grandchild is started by the child's nested subagent, one level below the owner's subagent tool.
func TestGrandchildToolSmithReachesNoMoreThanTheOwnersAllowList(t *testing.T) {
	root := shortTempDir(t)
	smith := bridgeSettings(t, root)
	smith.Tools = []string{"read_image"}
	childAgent := nestingAgent("explore", "read_image", "subagent", "tool_smith")
	launcher := &fakeLauncher{}
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
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
	got := runScriptThroughCatalog(t, launcher.requests[1].Tools, root, "read_image")
	if !strings.Contains(got, "read_image:invalid_input:tool read_image not allowed") {
		t.Fatalf("the grandchild's script reached read outside the owner's allow list: %s", got)
	}
}

// A tool_smith script at depth 3 is limited by the owner's allow list too. The owner's allow hides read, and the
// great-grandchild lists read, so its script is refused read as not allowed. The chain is owner, child, grandchild,
// great-grandchild, each started by the level above's nested subagent.
func TestDepthThreeToolSmithReachesNoMoreThanTheOwnersAllowList(t *testing.T) {
	root := shortTempDir(t)
	smith := bridgeSettings(t, root)
	smith.Tools = []string{"read_image"}
	childAgent := nestingAgent("explore", "read_image", "subagent", "tool_smith")
	launcher := &fakeLauncher{}
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 3, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
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
	got := runScriptThroughCatalog(t, launcher.requests[2].Tools, root, "read_image")
	if !strings.Contains(got, "read_image:invalid_input:tool read_image not allowed") {
		t.Fatalf("the depth-3 script reached read outside the owner's allow list: %s", got)
	}
}
