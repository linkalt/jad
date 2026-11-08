package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func openSet(t *testing.T, dir string, entries []string, summary int) *LazySet {
	t.Helper()
	set, err := OpenLazySet(dir, entries, summary, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestLazyFingerprintIsOrderAndDuplicateFree(t *testing.T) {
	a := LazyFingerprint([]string{"read", "mcp:docs:*", "read"}, 256)
	b := LazyFingerprint([]string{"mcp:docs:*", "read"}, 256)
	if a != b {
		t.Fatal("fingerprint changed with order or a duplicate entry")
	}
	if LazyFingerprint([]string{"read"}, 256) == LazyFingerprint([]string{"read"}, 128) {
		t.Fatal("fingerprint ignored the summary bound, which changes the summary the model sees")
	}
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

// A native and an MCP entry each bind only their own kind, and a second bind of the same list
// adds nothing, so no tool is listed or gated twice.
func TestBindIsIdempotentAndKeepsNativeAndMCPApart(t *testing.T) {
	set := openSet(t, t.TempDir(), []string{"read", "mcp:docs:*"}, 256)
	native := []Tool{lazyTool(t, "native.read", "native", "read"), lazyTool(t, "native.bash", "native", "bash")}
	mcp := []Tool{lazyTool(t, "mcp:docs:search", "mcp", "search")}
	set.Bind(native)
	set.Bind(mcp)
	set.Bind(mcp)
	set.Bind(native)
	if got := len(set.Entries()); got != 2 {
		t.Fatalf("bound %d tools, want 2 (read and the MCP search)", got)
	}
	for _, entry := range set.Entries() {
		if entry.Tool.Definition.ID == "native.bash" {
			t.Errorf("native.bash bound, but no entry names it")
		}
	}
}

// A wildcard entry that gains a tool refuses the new tool until it is loaded (rule 4a).
func TestWildcardGainingAToolRefusesTheNewToolUntilLoaded(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"mcp:docs:*"}, 256)
	set.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search")})
	if err := set.Load("search"); err != nil {
		t.Fatal(err)
	}
	// The server now exposes a second tool; the next prepare sees both, and the wildcard covers it.
	reopened := openSet(t, dir, []string{"mcp:docs:*"}, 256)
	reopened.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search"), lazyTool(t, "mcp:docs:fetch", "mcp", "fetch")})
	if !reopened.Loaded("mcp:docs:search") {
		t.Fatal("the loaded tool lost its state")
	}
	if reopened.Loaded("mcp:docs:fetch") {
		t.Fatal("a tool the wildcard gained was loaded without a load_tool call")
	}
}

// A changed config clears the loaded set and rewrites the file (rule 4b).
func TestConfigChangeClearsTheLoadedSet(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	if err := set.Load("read"); err != nil {
		t.Fatal(err)
	}
	changed := openSet(t, dir, []string{"read", "bash"}, 256)
	changed.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	if changed.Loaded("native.read") {
		t.Fatal("a changed tools.lazy kept the loaded set")
	}
	state, err := readLazyToolsState(filepath.Join(dir, lazyToolsFile))
	if err != nil || len(state.Loaded) != 0 || state.Fingerprint != LazyFingerprint([]string{"read", "bash"}, 256) {
		t.Fatalf("file after the change = %+v, %v; want empty loaded and the new fingerprint", state, err)
	}
}

// A loaded ID whose tool no longer exists is kept in the file and ignored by the run (rule 4c).
func TestLoadedIDThatNoLongerExistsDoesNotBreakTheRun(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"mcp:docs:*"}, 256)
	set.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search"), lazyTool(t, "mcp:docs:gone", "mcp", "gone")})
	if err := set.Load("gone"); err != nil {
		t.Fatal(err)
	}
	// The server no longer lists "gone".
	after := openSet(t, dir, []string{"mcp:docs:*"}, 256)
	after.Bind([]Tool{lazyTool(t, "mcp:docs:search", "mcp", "search")})
	if len(after.Entries()) != 1 {
		t.Fatalf("bound %d tools, want 1", len(after.Entries()))
	}
	if err := after.Load("search"); err != nil {
		t.Fatalf("a run with a stale loaded ID could not load a live tool: %v", err)
	}
	state, _ := readLazyToolsState(filepath.Join(dir, lazyToolsFile))
	if !containsID(state.Loaded, "mcp:docs:gone") {
		t.Errorf("the stale loaded ID was dropped from the file: %v", state.Loaded)
	}
}

// Rule 2: a name no bound tool has is refused and nothing is written.
func TestLoadRefusesAnUnknownNameWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	before, err := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Load("bash"); err == nil || !strings.Contains(err.Error(), "bash is not a lazy tool") {
		t.Fatalf("Load(bash) = %v, want a refusal naming bash", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if string(before) != string(after) {
		t.Fatal("a refused load changed the file")
	}
}

// Rule 6: a corrupt or missing file gives an empty set, and the file is rewritten from it.
func TestCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lazyToolsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	if set.Loaded("native.read") {
		t.Fatal("a corrupt file loaded a tool")
	}
	state, err := readLazyToolsState(filepath.Join(dir, lazyToolsFile))
	if err != nil || state.Fingerprint != LazyFingerprint([]string{"read"}, 256) {
		t.Fatalf("file after a corrupt read = %+v, %v; want it rewritten with the current fingerprint", state, err)
	}
}

func TestMissingFileGivesAnEmptySet(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	if set.Loaded("native.read") {
		t.Fatal("a missing file loaded a tool")
	}
	if _, err := os.Stat(filepath.Join(dir, lazyToolsFile)); err != nil {
		t.Fatalf("the file was not written for the empty set: %v", err)
	}
}

// Without a state folder (no catalog layout) the set cannot be opened.
func TestOpenLazySetNeedsAStateFolder(t *testing.T) {
	if _, err := OpenLazySet("", []string{"read"}, 256, nil, nil); err == nil || !strings.Contains(err.Error(), "catalog layout") {
		t.Fatalf("OpenLazySet with no state folder: %v, want the catalog-layout error", err)
	}
}

func containsID(ids []string, id string) bool {
	for _, value := range ids {
		if value == id {
			return true
		}
	}
	return false
}

// Batch load: every matched name is loaded with one write, and an unknown name is left out
// without stopping the others.
func TestLoadAllLoadsEveryMatchedNameOnce(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read", "mcp:docs:search", "bash"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read"), lazyTool(t, "mcp:docs:search", "mcp", "search"), lazyTool(t, "native.bash", "native", "bash")})
	matched, err := set.LoadAll([]string{"search", "nope", "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 3 || !matched[0] || matched[1] || !matched[2] {
		t.Fatalf("matched = %v, want [true false true]", matched)
	}
	state, err := readLazyToolsState(filepath.Join(dir, lazyToolsFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Loaded) != 2 || state.Loaded[0] != "mcp:docs:search" || state.Loaded[1] != "native.read" {
		t.Fatalf("loaded = %v, want the two matched IDs in sorted order", state.Loaded)
	}
}

// An all-unknown batch writes nothing.
func TestLoadAllWritesNothingWhenNoNameMatches(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	before, err := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if err != nil {
		t.Fatal(err)
	}
	matched, err := set.LoadAll([]string{"bash", "grep"})
	if err != nil {
		t.Fatal(err)
	}
	if matched[0] || matched[1] {
		t.Fatalf("matched = %v, want none", matched)
	}
	after, _ := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if string(before) != string(after) {
		t.Fatal("an all-unknown batch changed the file")
	}
}

// A name that is already loaded still matches, and the file is not rewritten for it.
func TestLoadAllReloadingWritesNothing(t *testing.T) {
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	set.Bind([]Tool{lazyTool(t, "native.read", "native", "read")})
	if _, err := set.LoadAll([]string{"read"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	matched, err := set.LoadAll([]string{"read"})
	if err != nil || !matched[0] {
		t.Fatalf("LoadAll again: matched %v, err %v", matched, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if string(before) != string(after) {
		t.Fatal("reloading an already loaded name changed the file")
	}
}

// lazySetWithFour binds a native read and bash, and two MCP tools of server docs, search and
// fetch, with the entries given. The profile shows every tool unless shown says otherwise.
func lazySetWithFour(t *testing.T, dir string, entries []string, shown func(Definition) bool) *LazySet {
	t.Helper()
	set, err := OpenLazySet(dir, entries, 256, shown, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Bind([]Tool{
		lazyTool(t, "native.read", "native", "read"),
		lazyTool(t, "native.bash", "native", "bash"),
		lazyTool(t, "mcp:docs:search", "mcp", "search"),
		lazyTool(t, "mcp:docs:fetch", "mcp", "fetch"),
	})
	return set
}

// Expansion: mcp:<server>:* selects every bound lazy tool of that server, sorted by ID, and nothing
// from another server or a native tool.
func TestMatchExpandsAServerPattern(t *testing.T) {
	set := lazySetWithFour(t, t.TempDir(), []string{"read", "bash", "mcp:docs:*"}, nil)
	matches := set.Match([]string{"mcp:docs:*"})
	if len(matches) != 1 || len(matches[0]) != 2 {
		t.Fatalf("matches = %v, want the two docs tools", matches)
	}
	if matches[0][0].Tool.Definition.ID != "mcp:docs:fetch" || matches[0][1].Tool.Definition.ID != "mcp:docs:search" {
		t.Errorf("order = %s, %s; want sorted by ID", matches[0][0].Tool.Definition.ID, matches[0][1].Tool.Definition.ID)
	}
}

// "*" alone selects every bound lazy tool; a non-lazy or hidden tool is never bound, so never matched.
func TestMatchStarSelectsOnlyBoundLazyTools(t *testing.T) {
	hidden := func(definition Definition) bool { return definition.ID != "mcp:docs:fetch" }
	set := lazySetWithFour(t, t.TempDir(), []string{"read", "mcp:docs:search", "mcp:docs:fetch"}, hidden)
	matches := set.Match([]string{"*"})
	if got := len(matches[0]); got != 2 {
		t.Fatalf("* selected %d tools, want 2 (read and search; fetch is hidden, bash is not lazy)", got)
	}
	for _, entry := range matches[0] {
		if entry.Tool.Definition.ID == "native.bash" || entry.Tool.Definition.ID == "mcp:docs:fetch" {
			t.Errorf("* selected %s, which is not a lazy tool the profile shows", entry.Tool.Definition.ID)
		}
	}
}

// Overlap: two patterns that select the same tool load it once in one write, and the matched count
// counts it once.
func TestLoadMatchesCountsAnOverlapOnce(t *testing.T) {
	dir := t.TempDir()
	set := lazySetWithFour(t, dir, []string{"mcp:docs:*", "mcp:docs:search"}, nil)
	matches := set.Match([]string{"mcp:docs:*", "mcp:docs:search"})
	added, err := set.LoadMatches(matches)
	if err != nil || added != 2 {
		t.Fatalf("LoadMatches = %d, %v; want 2 new tools", added, err)
	}
	state, err := readLazyToolsState(filepath.Join(dir, lazyToolsFile))
	if err != nil || len(state.Loaded) != 2 {
		t.Fatalf("file = %+v, %v; want two loaded IDs once each", state, err)
	}
}

// A pattern that selects nothing loads nothing and writes nothing.
func TestLoadMatchesWritesNothingWhenNothingIsSelected(t *testing.T) {
	dir := t.TempDir()
	set := lazySetWithFour(t, dir, []string{"read"}, nil)
	before, err := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if err != nil {
		t.Fatal(err)
	}
	added, err := set.LoadMatches(set.Match([]string{"mcp:docs:*"}))
	if err != nil || added != 0 {
		t.Fatalf("LoadMatches = %d, %v; want 0 and no error", added, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, lazyToolsFile))
	if string(before) != string(after) {
		t.Fatal("a pattern that selected nothing changed the file")
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
	dir := t.TempDir()
	set := openSet(t, dir, []string{"read"}, 256)
	native := lazyTool(t, "native.bash", "native", "bash")
	read := lazyTool(t, "native.read", "native", "read")
	set.Bind([]Tool{read})
	gatedRead, _ := set.Gate(read)
	catalog, err := NewCatalog([]Tool{native, gatedRead})
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
		t.Fatalf("before load = %v, want only bash", got)
	}
	if err := set.Load("read"); err != nil {
		t.Fatal(err)
	}
	after, err := catalog.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "bash" {
		t.Fatalf("after load = %v, want only bash: a lazy tool is never listed", got)
	}
	if len(before) != len(after) || !bytes.Equal(before[0], after[0]) {
		t.Fatalf("the tools array changed with the loaded set: %s then %s", before, after)
	}
	if listed := catalog.ListedDefinitions(); len(listed) != 1 || listed[0].Name != "bash" {
		t.Fatalf("ListedDefinitions = %v, want only bash", listed)
	}
}

// A refused call hands the gate's own wrappers the arguments the model sent, so a pre_tool
// command sees them. The arguments are not checked against the real schema.
func TestRefusedCallPassesItsRealArgumentsToTheWrappers(t *testing.T) {
	set := openSet(t, t.TempDir(), []string{"read"}, 256)
	real := lazyTool(t, "native.read", "native", "read")
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
	set := openSet(t, t.TempDir(), []string{"read"}, 256)
	real := lazyTool(t, "native.read", "native", "read")
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
	if !strings.Contains(string(direct), "tool_not_loaded") || !strings.Contains(string(direct), "load_tool with run") || ran != 0 {
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
	if strings.Contains(string(via), "tool_not_loaded") || !strings.Contains(string(via), "read it") || ran != 1 {
		t.Fatalf("a via-runner call = %s (ran %d), want the tool's own result", via, ran)
	}
	invalid, _ := catalog.ExecuteFunctionCall(context.Background(), runnerCall("read", `{}`), ExecutionContext{ViaRunner: true})
	if !strings.Contains(string(invalid), "validation_failed") || ran != 1 {
		t.Fatalf("a via-runner call with bad arguments = %s, want the real schema's refusal and no run", invalid)
	}
}
