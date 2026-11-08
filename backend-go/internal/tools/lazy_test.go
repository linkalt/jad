package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// lazyTool is a registered tool with the given provider-facing name and source.
func lazyTool(t *testing.T, id, source, name string) Tool {
	t.Helper()
	validator, err := CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	return Tool{
		Definition: Definition{ID: id, Source: source, Name: name, Description: name + " does a thing.\nSecond line.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator,
		Execute:    func(context.Context, map[string]any, ExecutionContext) (string, error) { return "", nil },
	}
}

func openSet(t *testing.T, entries []string, summary int) *LazySet {
	t.Helper()
	return NewLazySet(entries, summary, nil)
}

func TestLazySummaryCutsOnARuneBoundary(t *testing.T) {
	got := LazySummary("ééé first line\nsecond", 3)
	if got != "é" {
		t.Fatalf("LazySummary = %q, want the whole rune that fits in 3 bytes", got)
	}
	if LazySummary("  plain  \nmore", 256) != "plain" {
		t.Fatal("summary is the trimmed first line")
	}
}

// An MCP and an extension entry bind their own tools, a native entry binds nothing, and a second bind of
// the same list adds nothing, so no tool is listed or gated twice.
func TestBindIsIdempotentAndBindsOnlyMCPAndExtensionTools(t *testing.T) {
	set := openSet(t, []string{"read", "web_search", "mcp:docs:*"}, 256)
	native := []Tool{lazyTool(t, "native.read", "native", "read"), lazyTool(t, "native.bash", "native", "bash")}
	rest := []Tool{lazyTool(t, "mcp:docs:search", "mcp", "search"), lazyTool(t, "extension.web_search", "extension", "web_search")}
	set.Bind(native)
	set.Bind(rest)
	set.Bind(rest)
	set.Bind(native)
	if got := len(set.Entries()); got != 2 {
		t.Fatalf("bound %d tools, want 2 (the extension web_search and the MCP search)", got)
	}
	for _, entry := range set.Entries() {
		if entry.Tool.Definition.Source == "native" {
			t.Errorf("%s bound, but a native tool is never lazy", entry.Tool.Definition.ID)
		}
	}
}

// A native tool is never bound, gated or matched, even when an entry names it exactly, the profile shows it
// and something hands it to Bind, Gate or GateFor.
func TestANativeToolCanNeverBeLazy(t *testing.T) {
	set := openSet(t, []string{"read", "exec", "skill", "subagent", "load_tool"}, 256)
	for _, name := range []string{"read", "exec", "skill", "subagent", "load_tool"} {
		native := lazyTool(t, "native."+name, "native", name)
		set.Bind([]Tool{native})
		if gated, ok := set.Gate(native); ok || gated.Refused != nil {
			t.Errorf("Gate(%s) gated a native tool", name)
		}
		if gated, ok := set.GateFor(native); ok || gated.Refused != nil {
			t.Errorf("GateFor(%s) gated a native tool", name)
		}
		if matches := set.Match([]string{name, "*"}); len(matches[0]) != 0 || len(matches[1]) != 0 {
			t.Errorf("Match selected the native tool %s: %v", name, matches)
		}
	}
	if len(set.Entries()) != 0 {
		t.Fatalf("bound %v, want nothing", set.Entries())
	}
}

// A native tool handed over under an ID a bound tool has is not gated through that binding.
func TestGateForRefusesANativeToolWithABoundID(t *testing.T) {
	set := openSet(t, []string{"mcp:docs:search"}, 256)
	set.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search")})
	impostor := lazyTool(t, "mcp:docs:search", "native", "search")
	if _, ok := set.GateFor(impostor); ok {
		t.Fatal("GateFor gated a native tool that carries a bound tool's ID")
	}
}

// Without a state folder or a file: the set keeps nothing outside the process.
func TestLazySetNeedsNoStateFolder(t *testing.T) {
	set := NewLazySet([]string{"mcp:docs:search"}, 256, nil)
	set.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search")})
	if len(set.Entries()) != 1 {
		t.Fatalf("bound %v, want the one MCP tool", set.Entries())
	}
}

// lazySetWithFive binds a native read and bash (which no entry can make lazy), an extension web_search, and two
// MCP tools of server docs, search and fetch, with the entries given. The profile shows every tool unless
// shown says otherwise.
func lazySetWithFive(t *testing.T, entries []string, shown func(Definition) bool) *LazySet {
	t.Helper()
	set := NewLazySet(entries, 256, shown)
	set.Bind([]Tool{
		lazyTool(t, "native.read", "native", "read"),
		lazyTool(t, "native.bash", "native", "bash"),
		lazyTool(t, "extension.web_search", "extension", "web_search"),
		lazyTool(t, "mcp:docs:search", "mcp", "search"),
		lazyTool(t, "mcp:docs:fetch", "mcp", "fetch"),
	})
	return set
}

// Expansion: mcp:<server>:* selects every bound lazy tool of that server, sorted by ID, and nothing
// from another server, an extension tool or a native tool.
func TestMatchExpandsAServerPattern(t *testing.T) {
	set := lazySetWithFive(t, []string{"read", "bash", "web_search", "mcp:docs:*"}, nil)
	matches := set.Match([]string{"mcp:docs:*"})
	if len(matches) != 1 || len(matches[0]) != 2 {
		t.Fatalf("matches = %v, want the two docs tools", matches)
	}
	if matches[0][0].Tool.Definition.ID != "mcp:docs:fetch" || matches[0][1].Tool.Definition.ID != "mcp:docs:search" {
		t.Errorf("order = %s, %s; want sorted by ID", matches[0][0].Tool.Definition.ID, matches[0][1].Tool.Definition.ID)
	}
}

// "*" alone selects every bound lazy tool, MCP and extension; a native, non-lazy or hidden tool is never
// bound, so never matched.
func TestMatchStarSelectsOnlyBoundMCPAndExtensionTools(t *testing.T) {
	hidden := func(definition Definition) bool { return definition.ID != "mcp:docs:fetch" }
	set := lazySetWithFive(t, []string{"read", "bash", "web_search", "mcp:docs:search", "mcp:docs:fetch"}, hidden)
	matches := set.Match([]string{"*"})
	got := map[string]bool{}
	for _, entry := range matches[0] {
		got[entry.Tool.Definition.ID] = true
	}
	if len(got) != 2 || !got["extension.web_search"] || !got["mcp:docs:search"] {
		t.Fatalf("* selected %v, want the extension web_search and the MCP search (fetch is hidden; read and bash are native)", got)
	}
}

// IsLazyPattern tells a pattern from one tool name: "*" and mcp:<server>:* are patterns.
func TestIsLazyPattern(t *testing.T) {
	for name, want := range map[string]bool{"*": true, "mcp:docs:*": true, "mcp:docs:search": false, "read": false, "mcp:docs*": false} {
		if got := IsLazyPattern(name); got != want {
			t.Errorf("IsLazyPattern(%q) = %v, want %v", name, got, want)
		}
	}
}

// The tools array never holds a lazy tool, loaded or not, so the bytes the provider caches do not
// change with the set. A dispatch by name still reaches the tool, which the gate refuses unless a
// runner dispatches it.
func TestResponseToolsNeverListsALazyTool(t *testing.T) {
	set := openSet(t, []string{"mcp:docs:search"}, 256)
	native := lazyTool(t, "native.bash", "native", "bash")
	search := lazyTool(t, "mcp:docs:search", "mcp", "search")
	set.Bind([]Tool{search})
	gatedSearch, _ := set.Gate(search)
	catalog, err := NewCatalog([]Tool{native, gatedSearch})
	if err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		encoded, err := catalog.ResponseTools()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, raw := range encoded {
			var entry struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatal(err)
			}
			out = append(out, entry.Name)
		}
		return out
	}
	before, err := catalog.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "bash" {
		t.Fatalf("before = %v, want only bash", got)
	}
	set.Bind([]Tool{search})
	after, err := catalog.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "bash" {
		t.Fatalf("after a second bind = %v, want only bash: a lazy tool is never listed", got)
	}
	if len(before) != len(after) || !bytes.Equal(before[0], after[0]) {
		t.Fatalf("the tools array changed with the set: %s then %s", before, after)
	}
	if listed := eagerRows(catalog); len(listed) != 1 || listed[0].Name != "bash" {
		t.Fatalf("eager route rows = %v, want only bash", listed)
	}
}

// A refused call hands the gate's own wrappers the arguments the model sent, so a pre_tool
// command sees them. The arguments are not checked against the real schema.
func TestRefusedCallPassesItsRealArgumentsToTheWrappers(t *testing.T) {
	set := openSet(t, []string{"mcp:docs:read"}, 256)
	real := lazyTool(t, "mcp:docs:read", "mcp", "read")
	real.Definition.InputSchema = json.RawMessage(`{"type":"object","required":["path"]}`)
	set.Bind([]Tool{real})
	var seen map[string]any
	gated, ok := set.Gate(real)
	if !ok {
		t.Fatal("the set did not gate the lazy tool")
	}
	wrapped := gated
	wrapped.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
		seen = arguments
		return gated.Execute(ctx, arguments, execution)
	}
	catalog, err := NewCatalog([]Tool{wrapped})
	if err != nil {
		t.Fatal(err)
	}
	call := json.RawMessage(`{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"notes.txt\"}"}`)
	if _, err := catalog.ExecuteFunctionCall(context.Background(), call, ExecutionContext{}); err != nil {
		t.Fatal(err)
	}
	if seen["path"] != "notes.txt" {
		t.Fatalf("the wrapper saw %v for a refused call, want the call's arguments {path: notes.txt}", seen)
	}
}

// A lazy tool is refused when the model names it, with a pointer to load_tool run, and runs when
// a runner dispatches it (ViaRunner). It is never listed, and a runner's call is checked against
// the real schema like an eager tool's.
func TestLazyGateRefusesADirectCallAndRunsAViaRunnerCall(t *testing.T) {
	set := openSet(t, []string{"mcp:docs:read"}, 256)
	real := lazyTool(t, "mcp:docs:read", "mcp", "read")
	real.Definition.InputSchema = json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
	validator, err := CompileSchema(real.Definition.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	real.Validator = validator
	ran := 0
	real.Execute = func(context.Context, map[string]any, ExecutionContext) (string, error) {
		ran++
		return "read it", nil
	}
	set.Bind([]Tool{real})
	gated, ok := set.Gate(real)
	if !ok {
		t.Fatal("the set did not gate the lazy tool")
	}
	if gated.Listed == nil || gated.Listed() {
		t.Fatal("a lazy tool must never be listed")
	}
	catalog, err := NewCatalog([]Tool{gated})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := catalog.ExecuteFunctionCall(context.Background(), runnerCall("read", `{"path":"a"}`), ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(direct), "lazy_tool_not_loaded") || !strings.Contains(string(direct), "load_tool with run") || ran != 0 {
		t.Fatalf("a direct call = %s (ran %d), want the refusal that points at load_tool run", direct, ran)
	}
	// Even a call that breaks the real schema is answered by the refusal, not by validation.
	if bad, _ := catalog.ExecuteFunctionCall(context.Background(), runnerCall("read", `{}`), ExecutionContext{}); strings.Contains(string(bad), "validation_failed") {
		t.Errorf("a direct call with bad arguments reached the real schema: %s", bad)
	}
	via, err := catalog.ExecuteFunctionCall(context.Background(), runnerCall("read", `{"path":"a"}`), ExecutionContext{ViaRunner: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(via), "lazy_tool_not_loaded") || !strings.Contains(string(via), "read it") || ran != 1 {
		t.Fatalf("a via-runner call = %s (ran %d), want the tool's own result", via, ran)
	}
	invalid, _ := catalog.ExecuteFunctionCall(context.Background(), runnerCall("read", `{}`), ExecutionContext{ViaRunner: true})
	if !strings.Contains(string(invalid), "validation_failed") || ran != 1 {
		t.Fatalf("a via-runner call with bad arguments = %s, want the real schema's refusal and no run", invalid)
	}
}

// The tools route lists a lazy tool, marked lazy, beside the eager ones; the provider's tools array holds the
// eager ones only, byte for byte the same as a catalog that never had the lazy tool, and the lazy marker is no
// part of Definition, so it cannot reach the payload.
func TestRouteDefinitionsMarkLazyToolsAndLeaveTheProviderArrayAlone(t *testing.T) {
	set := openSet(t, []string{"mcp:docs:search"}, 256)
	eager := lazyTool(t, "native.bash", "native", "bash")
	search := lazyTool(t, "mcp:docs:search", "mcp", "search")
	fetch := lazyTool(t, "mcp:docs:fetch", "mcp", "fetch")
	set.Bind([]Tool{search})
	gated, ok := set.Gate(search)
	if !ok {
		t.Fatal("the set did not gate the lazy tool")
	}
	withLazy, err := NewCatalog([]Tool{eager, gated, fetch})
	if err != nil {
		t.Fatal(err)
	}
	without, err := NewCatalog([]Tool{eager, fetch})
	if err != nil {
		t.Fatal(err)
	}
	got, err := withLazy.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	want, err := without.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("tools array has %d entries, want %d", len(got), len(want))
	}
	for index := range got {
		if !bytes.Equal(got[index], want[index]) {
			t.Errorf("tool %d differs from the catalog without the lazy tool:\n%s\n%s", index, got[index], want[index])
		}
	}
	rows := withLazy.RouteDefinitions()
	if len(rows) != 3 || rows[0].Lazy || !rows[1].Lazy || rows[1].ID != "mcp:docs:search" || rows[2].Lazy {
		t.Fatalf("route rows = %+v, want bash, the lazy search, fetch in registry order", rows)
	}
	if listed := eagerRows(withLazy); len(listed) != 2 {
		t.Fatalf("eager route rows = %v, want the two eager tools", listed)
	}
	encoded, err := json.Marshal(search.Definition)
	if err != nil || strings.Contains(string(encoded), "lazy") {
		t.Fatalf("Definition encodes as %s (%v): the lazy flag must not be part of it", encoded, err)
	}
	eagerRow, _ := json.Marshal(rows[0])
	lazyRow, _ := json.Marshal(rows[1])
	if strings.Contains(string(eagerRow), `"lazy"`) || !strings.Contains(string(lazyRow), `"lazy":true`) {
		t.Fatalf("rows encode as %s and %s", eagerRow, lazyRow)
	}
}

// eagerRows is the route rows of the tools the model's tools array holds: every row not marked lazy.
func eagerRows(catalog *Catalog) []RouteDefinition {
	var rows []RouteDefinition
	for _, row := range catalog.RouteDefinitions() {
		if !row.Lazy {
			rows = append(rows, row)
		}
	}
	return rows
}
