package native

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// lateToolSmithCatalog builds the main catalog with tool_smith naming the given tools, the profile's allow
// list (nil for no allow block), deny list and lazy list. profile is built after the early native list, so
// this is the path that decides whether a script can reach it.
func lateToolSmithCatalog(t *testing.T, root string, names, allow, deny, lazy []string) (*tools.Catalog, *tools.LazySet) {
	t.Helper()
	set := tools.NewLazySet(lazy, 256, nil)
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

// A script can name a late tool the profile allows. A denied late tool is refused as not allowed. A native tool is
// never lazy, so a late tool that a lazy entry names (the loader refuses such an entry; the set is handed it here)
// is called like any other.
func TestToolSmithScriptReachesAnAllowedLateToolOnly(t *testing.T) {
	root := shortTempDir(t)
	catalog, set := lateToolSmithCatalog(t, root, []string{"read_image", "profile", "transcript"}, nil, []string{"transcript"}, []string{"profile"})
	got := runScriptThroughCatalog(t, catalog, root, "profile", "transcript")
	if strings.Contains(got, "is lazy") || strings.Contains(got, "profile:invalid_input:tool profile not allowed") {
		t.Fatalf("the native late tool profile was refused: %s", got)
	}
	if !strings.Contains(got, "transcript") || !strings.Contains(got, "not allowed") {
		t.Fatalf("the denied late tool transcript was not refused as not allowed: %s", got)
	}
	if len(set.Entries()) != 0 {
		t.Fatalf("the set bound %v, want no native tool", set.Entries())
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

// lazyExtensionSmithCatalog builds a catalog whose profile makes both extension tools lazy, with tool_smith naming
// scriptTools, and a local searxng-style server behind web_search that counts its requests.
func lazyExtensionSmithCatalog(t *testing.T, root string, scriptTools []string) (*tools.Catalog, *atomic.Int32) {
	t.Helper()
	searches := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		searches.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"title":"Lazy search hit","url":"https://example.com/hit","content":"found"}]}`))
	}))
	t.Cleanup(server.Close)
	lazy := []string{"web_search", "decide_systemone"}
	set := tools.NewLazySet(lazy, 256, nil)
	smith := bridgeSettings(t, root)
	smith.Tools = scriptTools
	sample := filepath.Join("..", "..", "..", "..", "sample", "tools")
	config := sessionfs.SessionConfig{
		SettingPath:              filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:      []string{filepath.Join(sample, "native", "tool_smith.json"), filepath.Join(sample, "extension", "web_search.json"), filepath.Join(sample, "extension", "decide_systemone.json")},
		NativeToolSmith:          &smith,
		ExtensionWebSearch:       &sessionfs.ExtensionWebSearchSettings{Provider: sessionfs.WebSearchProviderSearXNG, BaseURL: server.URL + "/search", DefaultResults: 3, MaxResults: 5, TimeoutMS: 5000, MaxResponseBytes: 4096, MaxSnippetChars: 100, MaxQueryChars: 100, UserAgent: "test", AllowedDomains: []string{}, BlockedDomains: []string{}},
		ExtensionDecideSystemone: &sessionfs.ExtensionDecideSystemoneSettings{BaseURL: "http://127.0.0.1:1", APIKeyEnv: "K", Model: "m", TimeoutMS: 1000, MaxQuestions: 1, MaxInputBytes: 10, MaxResponseBytes: 10},
		Profile:                  "main",
		Profiles:                 []string{"main"},
		ProfileTools:             sessionfs.ProfileToolSettings{Restricted: true, Allow: []string{}, Lazy: lazy},
	}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, nil, nil, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	if len(set.Entries()) != 2 {
		t.Fatalf("the set bound %d tools, want both lazy extension tools", len(set.Entries()))
	}
	return catalog, searches
}

func runScriptCode(t *testing.T, catalog *tools.Catalog, root, code string) string {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"code": code})
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "smith-lazy", "name": "tool_smith", "arguments": string(arguments)})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{CallID: "smith-lazy", WorkspaceRoot: root, SessionDir: root, SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A script's nested call of a lazy extension tool that tools.native.tool_smith.tools names runs: the bridge's
// allowlist is the gate, and the dispatch is a runner's. A lazy tool the script's list does not name is refused
// as not allowed and never runs, and the model's own direct call of the lazy tool is still refused.
func TestToolSmithScriptRunsAnAllowlistedLazyExtensionToolOnly(t *testing.T) {
	root := shortTempDir(t)
	catalog, searches := lazyExtensionSmithCatalog(t, root, []string{"web_search"})
	got := runScriptCode(t, catalog, root, "const a = await tools.web_search({query: 'go'});\nconst b = await tools.decide_systemone({});\nreturn a.status + ':' + a.summary + '|' + b.status + ':' + b.summary;")
	if !strings.Contains(got, "ok:1 results for") {
		t.Fatalf("the allowlisted lazy web_search did not run from the script: %s", got)
	}
	if !strings.Contains(got, "tool decide_systemone not allowed") {
		t.Fatalf("the lazy decide_systemone, not on the script's list, was not refused as not allowed: %s", got)
	}
	if searches.Load() != 1 {
		t.Fatalf("the search server saw %d requests, want 1", searches.Load())
	}
	if direct := executeNamedCall(t, catalog, "web_search", `{"query":"go"}`); !strings.Contains(direct, "lazy_tool_not_loaded") || searches.Load() != 1 {
		t.Fatalf("the model's direct call of the lazy tool was not refused (searches %d): %s", searches.Load(), direct)
	}
}
