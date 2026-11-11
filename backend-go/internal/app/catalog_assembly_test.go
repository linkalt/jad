package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// assemblyFixture is review/reviewer with the owner's stdio MCP server (tools ask, lookup, ...),
// load_tool and the server's ask lazy, a hook matching a server's tool, and three agents: finder
// (the owner's tools), scout (the toolset extra/search, with its own copy of the server and a hook
// of its own) and lister (an allow list). Its config is the one ListTools loads; every start of
// the server (the owner's and the toolset's copy) appends a line to the returned file.
func assemblyFixture(t *testing.T) (*layoutFixture, sessionfs.SessionConfig, string) {
	t.Helper()
	f := samplingFixture(t)
	starts := filepath.Join(t.TempDir(), "starts")
	server, _ := json.Marshal(appStdioMCPServerFile(t, "", func(file map[string]any) {
		file["transport"].(map[string]any)["env"] = map[string]string{appStdioMCPEnvironment: "1", appStdioMCPAskEnvironment: "1", appStdioMCPStartLogEnvironment: starts}
		file["sampling"] = map[string]any{"enabled": true, "max_requests": 50}
	}))
	writeFile(t, filepath.Join(f.root, "mcp", "m", "stdio.json"), server)
	settingPath := filepath.Join(f.root, "setting.json")
	var setting map[string]any
	if data, err := os.ReadFile(settingPath); err != nil || json.Unmarshal(data, &setting) != nil {
		t.Fatalf("read %s: %v", settingPath, err)
	}
	libraries := setting["libraries"].(map[string]any)
	libraries["tool_definition_paths"] = append(libraries["tool_definition_paths"].([]any), filepath.Join(filepath.Dir(libraries["tool_definition_paths"].([]any)[0].(string)), "load_tool.json"))
	body, _ := json.Marshal(setting)
	writeFile(t, settingPath, body)
	writeFile(t, filepath.Join(f.root, "tools", "review", "stdio.json"), []byte(`{"native":{"async":{},"load_tool":{},"subagent":{"background_writers":true,"max_background_writers":2,`+
		`"agents":{"finder":{"description":"Uses the server."},"scout":{"description":"Searches.","toolset":"extra/search"},"lister":{"description":"Lists.","tools":["mcp:stdio:lookup"]}}}},`+
		`"mcp":{"m/stdio":{}},"hooks":{"enabled":true,"timeout_ms":5000,"post_tool":[{"name":"audit","argv":["true"],"match":"mcp:stdio:ask"}]}}`))
	writeFile(t, filepath.Join(f.root, "tools", "extra", "search.json"), []byte(`{"native":{"load_tool":{}},"mcp":{"m/stdio":{}},"hooks":{"enabled":true,"timeout_ms":5000,"pre_tool":[{"name":"gate","argv":["true"],"match":"*"}]}}`))
	f.overwrite(t, "profiles/review/reviewer.json", `{"models":["openai/large","openai/small"],"model_default":"openai/large","toolset":"review/stdio","tools":{"allow":["subagent","load_tool","mcp:stdio:ask","mcp:stdio:lookup"],"lazy":["mcp:stdio:ask"]}}`)
	// The root setting is read when the fixture starts; reload it for the load_tool definition.
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{WorkingDir: f.root, RootSettingPath: settingPath, Environment: map[string]string{"FIXTURE_KEY": "k"}})
	if err != nil {
		t.Fatal(err)
	}
	location, err := sessionfs.Resolve(loaded.Roots, "local:demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	return f, config, starts
}

// catalogListing is every registered definition and what the profile's model sees of the catalog
// (exposed definitions and hidden ids), sorted, as JSON: two builds that agree on it registered
// the same tools with the same names, descriptions and schemas, and narrowed them the same way.
func catalogListing(t *testing.T, config sessionfs.SessionConfig, registered *tools.Catalog) string {
	t.Helper()
	exposed, hidden, err := applyProfileTools(config, registered)
	if err != nil {
		t.Fatal(err)
	}
	sorted := func(definitions []tools.Definition) []tools.Definition {
		out := append([]tools.Definition(nil), definitions...)
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	body, err := json.MarshalIndent(map[string]any{"registered": sorted(registered.Definitions()), "exposed": sorted(exposed.Definitions()), "hidden": hidden}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func startLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func mcpManagerKeys(resolver *Resolver) []string {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	keys := make([]string, 0, len(resolver.mcpManagers))
	for key := range resolver.mcpManagers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// countingTools are tools whose every call adds one to calls before it runs the tool.
func countingTools(list []tools.Tool, calls *atomic.Int32) []tools.Tool {
	out := make([]tools.Tool, len(list))
	for i, tool := range list {
		if execute := tool.ExecuteOutput; execute != nil {
			tool.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
				calls.Add(1)
				return execute(ctx, arguments, execution)
			}
		}
		if execute := tool.Execute; execute != nil {
			tool.Execute = func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
				calls.Add(1)
				return execute(ctx, arguments, execution)
			}
		}
		out[i] = tool
	}
	return out
}

func listedDefinitions(list []tools.Tool) []tools.Definition {
	out := make([]tools.Definition, len(list))
	for i, tool := range list {
		out[i] = tool.Definition
	}
	return out
}

// R4a: a registered catalog is acquired (stores, native catalog, MCP managers and their started
// servers) and then assembled. Assembly over the acquired parts calls no tool, starts no server,
// opens or drops no MCP manager and leaves the listed tools as listed; what it builds is what the
// whole build (acquire and assemble) builds for the same config, which over the warm managers
// starts nothing either. The assembled catalog gates the parent's lazy MCP tool, and the subagent
// tool's agent list is redescribed after the MCP tools were bound (lister has only an MCP tool).
func TestCatalogAssemblyStartsNothingAndLeavesItsAcquiredPartsAlone(t *testing.T) {
	f, config, starts := assemblyFixture(t)
	resolver := f.service.resolver
	ctx := context.Background()
	parts, _, _, err := resolver.acquireCatalogParts(ctx, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := startLines(t, starts); got != 2 {
		t.Fatalf("acquiring started the server %d times, want 2 (the owner's and the toolset's copy)", got)
	}
	toolset, ok := parts.toolsetMCP["extra/search"]
	if !ok || len(parts.mcpTools) == 0 || len(toolset.Tools) == 0 {
		t.Fatalf("acquired parts: %d MCP tools, toolsets %v", len(parts.mcpTools), parts.toolsetMCP)
	}
	managers := mcpManagerKeys(resolver)
	calls := &atomic.Int32{}
	parts.mcpTools = countingTools(parts.mcpTools, calls)
	toolset.Tools = countingTools(toolset.Tools, calls)
	parts.toolsetMCP["extra/search"] = toolset
	listed, toolsetListed := listedDefinitions(parts.mcpTools), listedDefinitions(toolset.Tools)

	var warned []string
	registered, err := assembleRegisteredCatalog(config, parts, func(message string) { warned = append(warned, message) })
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("assembly called %d MCP tools", got)
	}
	if got := startLines(t, starts); got != 2 {
		t.Errorf("assembly started a server: %d starts, want 2", got)
	}
	if got := mcpManagerKeys(resolver); !reflect.DeepEqual(got, managers) {
		t.Errorf("assembly changed the resolver's MCP managers: %q, was %q", got, managers)
	}
	if !reflect.DeepEqual(listedDefinitions(parts.mcpTools), listed) || !reflect.DeepEqual(listedDefinitions(parts.toolsetMCP["extra/search"].Tools), toolsetListed) {
		t.Error("assembly changed the listed MCP tools")
	}
	if len(warned) != 0 {
		t.Errorf("warned %q", warned)
	}

	whole, _, _, err := resolver.buildRegisteredCatalog(ctx, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := catalogListing(t, config, registered), catalogListing(t, config, whole); got != want {
		t.Errorf("assembled catalog differs from the whole build:\n%s\nwant\n%s", got, want)
	}
	if got := startLines(t, starts); got != 2 {
		t.Errorf("the whole build over the warm managers started a server: %d starts, want 2", got)
	}

	ask := mcpToolNameFor("stdio", "ask")
	output, err := registered.ExecuteFunctionCall(ctx, json.RawMessage(`{"type":"function_call","call_id":"c1","name":"`+ask+`","arguments":"{}"}`), tools.ExecutionContext{CallID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "lazy_tool_not_loaded") || calls.Load() != 0 {
		t.Errorf("the parent's lazy %s was not gated (%d calls): %s", ask, calls.Load(), output)
	}
	var description string
	for _, definition := range registered.Definitions() {
		if definition.Name == "subagent" {
			description = definition.Description
		}
	}
	if !strings.Contains(description, "- lister [can change files") {
		t.Errorf("the agent list was not redescribed after binding the MCP tools:\n%s", description)
	}
	if parts.sections.Subagent == nil || parts.sections.Subagent.Definition.Description == description {
		t.Error("assembly redescribed the subagent section of its parts instead of its own copy")
	}
	// The owner is its native sections in their order, then the profile's MCP tools as listed.
	var want, got []string
	for _, tool := range append(parts.sections.Tools(), parts.mcpTools...) {
		want = append(want, tool.Definition.Name)
	}
	for _, definition := range registered.Definitions() {
		got = append(got, definition.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("owner order %q, want native sections then MCP %q", got, want)
	}
}

// Assembly reports through warn, not the resolver's logger: a hook naming a disabled MCP server
// is a warning, and a hook naming no registered tool refuses the catalog.
func TestCatalogAssemblyChecksTheHooksMatches(t *testing.T) {
	f, config, _ := assemblyFixture(t)
	assemble := func(change func(*sessionfs.SessionConfig)) ([]string, error) {
		t.Helper()
		parts, _, _, err := f.service.resolver.acquireCatalogParts(context.Background(), config, nil)
		if err != nil {
			t.Fatal(err)
		}
		changed := config
		changed.MCPServers = append([]sessionfs.MCPServerReference{{ID: "off", Enabled: false}}, config.MCPServers...)
		changed.Hooks.PreTool = append([]sessionfs.ToolEventHook(nil), config.Hooks.PreTool...)
		change(&changed)
		var warned []string
		_, err = assembleRegisteredCatalog(changed, parts, func(message string) { warned = append(warned, message) })
		return warned, err
	}
	warned, err := assemble(func(c *sessionfs.SessionConfig) {
		c.Hooks.PreTool = append(c.Hooks.PreTool, sessionfs.ToolEventHook{Name: "off", Match: "mcp:off:x", Argv: []string{"true"}})
	})
	if err != nil || len(warned) != 1 || !strings.Contains(warned[0], "names a disabled MCP server") {
		t.Errorf("hook on a disabled server: warned %q, err %v", warned, err)
	}
	if _, err := assemble(func(c *sessionfs.SessionConfig) {
		c.Hooks.PreTool = append(c.Hooks.PreTool, sessionfs.ToolEventHook{Name: "none", Match: "mcp:stdio:missing", Argv: []string{"true"}})
	}); err == nil || !strings.Contains(err.Error(), "not a registered tool") {
		t.Errorf("hook on no registered tool: err %v", err)
	}
}
