package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// lateToolSmithCatalog builds the main catalog with tool_smith naming the given tools, the profile's allow
// list (nil for no allow block), deny list and lazy list. profile is built after the early native list, so
// this is the path that decides whether a script can reach it.
func lateToolSmithCatalog(t *testing.T, root string, names, allow, deny, lazy []string) (*tools.Catalog, *tools.LazySet) {
	t.Helper()
	set, err := tools.OpenLazySet(t.TempDir(), lazy, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	smith := bridgeSettings(t, root)
	smith.Tools = names
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "tool_smith.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeToolSmith:     &smith,
		NativeProfile:       &sessionfs.NativeProfileSettings{},
		Profile:             "main",
		Profiles:            []string{"main", "reviewer"},
		ProfileTools:        sessionfs.ProfileToolSettings{Restricted: true, HasAllow: allow != nil, Allow: allow, Deny: deny, Lazy: lazy},
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, nil, nil, messenger, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	return catalog, set
}

// runScriptThroughCatalog runs a tool_smith script through the catalog's own tool_smith tool, and the script
// calls each named tool with empty arguments and returns "name:status" for each.
func runScriptThroughCatalog(t *testing.T, catalog *tools.Catalog, root string, names ...string) string {
	t.Helper()
	var calls []string
	for _, name := range names {
		calls = append(calls, "{ name: '"+name+"', r: await tools['"+name+"']({}) }")
	}
	body := "const out = [];\nfor (const c of [" + strings.Join(calls, ", ") + "]) out.push(c.name + ':' + c.r.status + ':' + c.r.summary);\nreturn out.join('|');"
	arguments, _ := json.Marshal(map[string]string{"code": body})
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "smith-late", "name": "tool_smith", "arguments": string(arguments)})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{CallID: "smith-late", WorkspaceRoot: root, SessionDir: root, SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A script can name a late tool the profile allows. A denied late tool is refused as not allowed, and a lazy
// late tool is refused before the owner's set loads it.
func TestToolSmithScriptReachesAnAllowedLateToolOnly(t *testing.T) {
	root := shortTempDir(t)
	catalog, set := lateToolSmithCatalog(t, root, []string{"read_image", "profile", "transcript"}, nil, []string{"transcript"}, []string{"profile"})
	before := runScriptThroughCatalog(t, catalog, root, "profile", "transcript")
	if !strings.Contains(before, "profile:error:Tool profile is lazy and not loaded yet") {
		t.Fatalf("the lazy late tool profile was not refused before load: %s", before)
	}
	if !strings.Contains(before, "transcript") || !strings.Contains(before, "not allowed") {
		t.Fatalf("the denied late tool transcript was not refused as not allowed: %s", before)
	}
	if err := set.Load("profile"); err != nil {
		t.Fatal(err)
	}
	after := runScriptThroughCatalog(t, catalog, root, "profile")
	if strings.Contains(after, "tool_not_loaded") || strings.Contains(after, "not allowed") {
		t.Fatalf("the loaded allowed late tool profile was still refused: %s", after)
	}
}

// The script reaches only what the profile's allow list shows the model, the same rule the model's listing
// uses. A late tool outside allow is refused as not allowed, and so is an early one.
func TestToolSmithScriptReachesOnlyWhatTheAllowListShows(t *testing.T) {
	root := shortTempDir(t)
	catalog, _ := lateToolSmithCatalog(t, root, []string{"read_image", "profile"}, []string{"tool_smith", "profile"}, nil, nil)
	got := runScriptThroughCatalog(t, catalog, root, "read_image", "profile")
	if !strings.Contains(got, "read_image:invalid_input:tool read_image not allowed") {
		t.Fatalf("read outside allow was not refused as not allowed: %s", got)
	}
	if strings.Contains(got, "tool profile not allowed") {
		t.Fatalf("the allowed profile was refused as not allowed: %s", got)
	}
	catalog, _ = lateToolSmithCatalog(t, root, []string{"read_image", "profile"}, []string{"tool_smith", "read_image"}, nil, nil)
	got = runScriptThroughCatalog(t, catalog, root, "profile")
	if !strings.Contains(got, "profile:invalid_input:") || !strings.Contains(got, "not allowed") {
		t.Fatalf("profile outside allow was not refused as not allowed: %s", got)
	}
}

// The lazy gate still applies to an allowed tool: a late tool that allow shows but the owner has not loaded
// is refused as not loaded, and runs once it is loaded.
func TestToolSmithAllowedLazyLateToolWaitsForLoad(t *testing.T) {
	root := shortTempDir(t)
	catalog, set := lateToolSmithCatalog(t, root, []string{"profile"}, []string{"tool_smith", "profile"}, nil, []string{"profile"})
	before := runScriptThroughCatalog(t, catalog, root, "profile")
	if !strings.Contains(before, "profile:error:Tool profile is lazy and not loaded yet") {
		t.Fatalf("the allowed lazy profile was not refused before load: %s", before)
	}
	if err := set.Load("profile"); err != nil {
		t.Fatal(err)
	}
	after := runScriptThroughCatalog(t, catalog, root, "profile")
	if strings.Contains(after, "lazy and not loaded") || strings.Contains(after, "not allowed") {
		t.Fatalf("the loaded allowed lazy profile was still refused: %s", after)
	}
}

// A child's tool_smith shares the owner's bound list, so the owner's allow list limits the child's script too.
// The owner's allow hides read, and the child lists read, so the child's script is refused read as not allowed.
func TestChildToolSmithReachesNoMoreThanTheOwnersAllowList(t *testing.T) {
	root := shortTempDir(t)
	smith := bridgeSettings(t, root)
	smith.Tools = []string{"read_image"}
	childAgent := nestingAgent("scripter", "read_image", "tool_smith")
	launcher := &fakeLauncher{}
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
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
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, nil, nil, nil, messenger, nil, nil)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"scripter"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	got := runScriptThroughCatalog(t, launcher.requests[0].Tools, root, "read_image")
	if !strings.Contains(got, "read_image:invalid_input:tool read_image not allowed") {
		t.Fatalf("the child's script reached read outside the owner's allow list: %s", got)
	}
}
