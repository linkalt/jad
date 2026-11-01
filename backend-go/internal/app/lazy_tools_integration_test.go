package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// lazyFixture is a session whose parent profile lists lazy tools in tools.lazy, scripted to
// make the given calls in order. It has the in-process MCP server with a "lookup" tool.
func lazyFixture(t *testing.T, lazy []string, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	return lazyFixtureWithProfile(t, map[string]any{"allow": []string{"read", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy}, steps)
}

// lazyFixtureWithMax is lazyFixture with tools.lazy_load_max set, for the batch limit.
func lazyFixtureWithMax(t *testing.T, lazy []string, max int, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	return lazyFixtureWithProfile(t, map[string]any{"allow": []string{"read", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy, "lazy_load_max": max}, steps)
}

func lazyFixtureWithProfile(t *testing.T, profileTools map[string]any, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	return lazyFixtureWithExtraTools(t, profileTools, nil, steps)
}

// lazyFixtureWithExtraTools is lazyFixtureWithProfile with more tools on the MCP server, each
// answering like lookup.
func lazyFixtureWithExtraTools(t *testing.T, profileTools map[string]any, extra []string, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls, extra: extra})
	f.provider.parentSteps = steps
	return f, calls
}

// parentRequests returns the requests the parent model received, in order.
func parentRequests(f subagentFixture) []providerRequest {
	return f.provider.forModel("parent-model")
}

// lastRequestText is the text of the parent's last request, which holds the whole history.
func lastRequestText(t *testing.T, requests []providerRequest) string {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("the parent made no request")
	}
	return requests[len(requests)-1].Text
}

// lazyFile is the state file of the profile's lazy set: the profile's own state folder.
func (f subagentFixture) lazyFile() string {
	return filepath.Join(f.stateDir, "lazy_tools.json")
}

// lazyFileLoaded reads the loaded list the state file records.
func lazyFileLoaded(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var state struct {
		Loaded []string `json:"loaded"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return state.Loaded
}

// A native lazy tool is refused before load and runs after load_tool loads it.
func TestLazyToolIsRefusedBeforeLoadAndRunsAfter(t *testing.T) {
	steps := []subagentStep{
		{"read", `{"path":"notes.txt"}`},
		{"load_tool", `{"names":["read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixture(t, []string{"read"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	// The last request holds the whole history: the refused call, the load and the call after it.
	history := lastRequestText(t, parentRequests(f))
	refused := strings.Index(history, `"call_id":"call-step-1"`)
	loaded := strings.Index(history, `"call_id":"call-step-2"`)
	ran := strings.Index(history, `"call_id":"call-step-3"`)
	if refused < 0 || loaded < 0 || ran < 0 {
		t.Fatalf("the history lacks one of the three calls:\n%s", history)
	}
	if !strings.Contains(history[refused:loaded], "tool_not_loaded") {
		t.Errorf("the first read was not refused:\n%s", history)
	}
	if strings.Contains(history[loaded:ran], "tool_not_loaded") {
		t.Errorf("load_tool itself was refused:\n%s", history)
	}
	if !strings.Contains(history[ran:], "the secret is 42") {
		t.Errorf("the read after load did not run:\n%s", history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 || loaded[0] != "native.read" {
		t.Errorf("loaded set = %v, want [native.read]", loaded)
	}
}

// The MCP tool follows the same path: refused before load, run after.
func TestLazyMCPToolIsRefusedBeforeLoadAndRunsAfter(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{name, `{"word":"answer"}`},
		{"load_tool", `{"names":["` + name + `"]}`},
		{name, `{"word":"answer"}`},
	}
	f, calls := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the first MCP call was not refused:\n%s", history)
	}
	if calls.Load() != 1 || !strings.Contains(history, "MCP says answer is 99") {
		t.Errorf("the MCP server saw %d calls (want 1); the history:\n%s", calls.Load(), history)
	}
}

// A lazy tool is absent from the tools array before load and present, with its real schema, from
// the next request after load_tool loaded it. load_tool is listed while a lazy tool is unloaded.
func TestLazyMCPToolIsAbsentBeforeLoadAndPresentWithItsRealSchemaAfter(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["` + name + `"]}`},
		{"load_tool", `{"names":["` + name + `"]}`},
	}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	f.run(t)
	parent := parentRequests(f)
	if len(parent) < 3 {
		t.Fatalf("parent made %d requests, want at least 3", len(parent))
	}
	if slices.Contains(parent[0].Tools, name) || !slices.Contains(parent[0].Tools, "load_tool") {
		t.Fatalf("before load tools = %v, want load_tool and no %s", parent[0].Tools, name)
	}
	if !slices.Contains(parent[1].Tools, name) || !slices.Contains(parent[1].Tools, "load_tool") {
		t.Errorf("after load tools = %v, want %s and load_tool (load_tool stays listed while enabled)", parent[1].Tools, name)
	}
	if description := parent[1].Descriptions[name]; description != "looks a word up" {
		t.Errorf("the listed MCP tool description = %q, want the real one (looks a word up)", description)
	}
	// The schema the model is shown is the server's inputSchema, compared as parsed JSON because
	// the SDK may re-encode it.
	want := map[string]any{"type": "object", "properties": map[string]any{"word": map[string]any{"type": "string"}}, "required": []any{"word"}, "additionalProperties": false}
	var got any
	if err := json.Unmarshal(parent[1].Parameters[name], &got); err != nil {
		t.Fatalf("decode the listed parameters %s: %v", parent[1].Parameters[name], err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the listed MCP parameters = %v, want the server inputSchema %v", got, want)
	}
}

// Decider ruling (a): the loaded check runs before the real schema. An unloaded lazy native call
// with arguments the real schema rejects is answered tool_not_loaded, not validation_failed, and
// its pre_tool hook still fires once.
func TestUnloadedLazyCallWithBadArgsIsRefusedBeforeValidation(t *testing.T) {
	steps := []subagentStep{{"read", `{}`}}
	f, marker := lazyHookFixture(t, []string{"read"}, steps, "read")
	f.run(t)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("an unloaded call with bad args was not refused:\n%s", history)
	}
	if strings.Contains(history, "validation_failed") {
		t.Errorf("an unloaded call with bad args was validated against the real schema:\n%s", history)
	}
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one refused call with bad args, want 1", got)
	}
}

// Decider ruling (b): with tools.native.load_tool enabled, load_tool is always listed in the tools
// array, even when no lazy tool is bound. With nothing bound, it reports "No lazy tools.".
func TestLoadToolIsListedWhenNoLazyToolIsBound(t *testing.T) {
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"read", "load_tool"}, "lazy": []string{}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.parentSteps = []subagentStep{{"load_tool", `{}`}}
	f.run(t)
	parent := parentRequests(f)
	if len(parent) == 0 {
		t.Fatal("the parent made no request")
	}
	if !slices.Contains(parent[0].Tools, "load_tool") {
		t.Errorf("load_tool was not listed with no lazy tool bound: tools = %v", parent[0].Tools)
	}
	if history := lastRequestText(t, parent); !strings.Contains(history, "No lazy tools.") {
		t.Errorf("load_tool did not report No lazy tools.:\n%s", history)
	}
}

// The tools array keeps catalog order, and it is the same order after a restart over the same
// state folder: a load adds its tool in place, it never reorders the rest.
func TestLazyToolsArrayIsInCatalogOrderAfterARestart(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["` + name + `"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	f.run(t)
	parent := parentRequests(f)
	if len(parent) < 2 {
		t.Fatalf("parent made %d requests, want at least 2", len(parent))
	}
	before := parent[len(parent)-1].Tools
	if slices.Index(before, name) < 0 || slices.Index(before, "read") > slices.Index(before, name) {
		t.Fatalf("tools before restart = %v, want read before %s in catalog order", before, name)
	}
	f.provider.mu.Lock()
	f.provider.parentSteps = []subagentStep{{"read", `{"path":"notes.txt"}`}}
	f.provider.requests = nil
	f.provider.mu.Unlock()
	f.apiURL = f.restart(t, nil)
	f.run(t)
	after := parentRequests(f)
	if len(after) == 0 {
		t.Fatal("no request after restart")
	}
	if got := after[0].Tools; !slices.Equal(got, before) {
		t.Errorf("tools after restart = %v, want the same catalog-ordered array %v", got, before)
	}
}

// Restart over the same state folder restores the loaded set. The session's history persists
// too, so the first run's results are still in it: the call after the restart is the fourth
// scripted step, and it must run without a refusal.
func TestLazyLoadIsRestoredAfterARestart(t *testing.T) {
	steps := []subagentStep{
		{"read", `{"path":"notes.txt"}`},
		{"load_tool", `{"names":["read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixture(t, []string{"read"}, steps)
	f.run(t)
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 {
		t.Fatalf("before restart loaded = %v", loaded)
	}
	f.provider.mu.Lock()
	f.provider.parentSteps = []subagentStep{{"read", `{"path":"notes.txt"}`}, {"read", `{"path":"notes.txt"}`}, {"read", `{"path":"notes.txt"}`}, {"read", `{"path":"notes.txt"}`}}
	f.provider.requests = nil
	f.provider.mu.Unlock()
	f.apiURL = f.restart(t, nil)
	f.run(t)
	history := lastRequestText(t, parentRequests(f))
	after := strings.LastIndex(history, `"call_id":"call-step-4"`)
	if after < 0 {
		t.Fatalf("the call after restart was not made:\n%s", history)
	}
	if strings.Contains(history[after:], "tool_not_loaded") || !strings.Contains(history[after:], "the secret is 42") {
		t.Errorf("the restored tool was refused or did not run after restart:\n%s", history[after:])
	}
}

// lazyHookFixture is lazyFixture plus a pre_tool hook on match that appends one line to a
// marker file for every fire, so a test can count the fires of each call.
func lazyHookFixture(t *testing.T, lazy []string, steps []subagentStep, match string) (subagentFixture, string) {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	marker := filepath.Join(t.TempDir(), "fires.txt")
	profileTools := map[string]any{"allow": []string{"read", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy}
	session := map[string]any{
		"hooks": map[string]any{"enabled": true, "pre_tool": []any{
			map[string]any{"name": "log", "match": match, "argv": []string{shell, "-c", "echo fired >> " + marker}},
		}},
	}
	session["__profile_extra"] = map[string]any{"tools": profileTools}
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": session, "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.parentSteps = steps
	return f, marker
}

// fires counts the lines a hook wrote to its marker file (0 when it never ran).
func fires(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "fired")
}

// A refused lazy native call still fires its pre_tool hook, once per call, and then returns
// tool_not_loaded. The hook is an ordinary tool-event hook, so refusal is visible to it.
func TestRefusedLazyNativeCallFiresItsHookOnce(t *testing.T) {
	steps := []subagentStep{{"read", `{"path":"notes.txt"}`}}
	f, marker := lazyHookFixture(t, []string{"read"}, steps, "read")
	f.run(t)
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one refused read, want 1", got)
	}
	if !strings.Contains(lastRequestText(t, parentRequests(f)), "tool_not_loaded") {
		t.Error("the call was not refused")
	}
}

// After load, a call to the lazy tool fires its hook exactly once: no double wrap.
func TestLoadedLazyNativeCallFiresItsHookOnce(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{"names":["read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, marker := lazyHookFixture(t, []string{"read"}, steps, "read")
	f.run(t)
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one loaded read, want 1", got)
	}
	if !strings.Contains(lastRequestText(t, parentRequests(f)), "the secret is 42") {
		t.Error("the loaded read did not run")
	}
}

// A lazy MCP tool, refused before load, fires its hook once and is refused.
func TestRefusedLazyMCPCallFiresItsHookOnce(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{{name, `{"word":"answer"}`}}
	f, marker := lazyHookFixture(t, []string{"mcp:fixture:lookup"}, steps, "mcp:fixture:lookup")
	f.run(t)
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one refused MCP call, want 1", got)
	}
}

// A lazy entry the profile does not show is a configuration error: a denied lazy tool could
// never be loaded or called, so it is refused rather than quietly absent. The check runs on
// the registered definitions, so it is tested on them directly.
func TestLazyEntryTheProfileHidesIsAConfigError(t *testing.T) {
	registered := []tools.Definition{
		{ID: "native.read", Source: "native", Name: "read"},
		{ID: "native.bash", Source: "native", Name: "bash"},
	}
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev"}
	for name, tc := range map[string]struct {
		settings sessionfs.ProfileToolSettings
		want     string
	}{
		"denied":          {sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{"read"}, Deny: []string{"read"}}, `tools.lazy[0] lists "read"`},
		"outside allow":   {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"bash"}, Lazy: []string{"read"}}, `tools.lazy[0] lists "read"`},
		"visible":         {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read"}, Lazy: []string{"read"}}, ""},
		"no allow, shown": {sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{"bash"}}, ""},
	} {
		err := checkLazyEntriesVisible(config, tc.settings, registered)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

// A child of a profile with tools.lazy gates its lazy MCP tools with the owning profile's set: its
// call before a load is refused as tool_not_loaded, and the MCP server never runs.
func TestChildOfAParentWithLazyToolsGatesTheLazyMCPTool(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "read", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"read", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{name, `{}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests", len(child))
	}
	history := child[len(child)-1].Text
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the child's call to the unloaded lazy tool was not refused:\n%s", history)
	}
	if calls.Load() != 0 {
		t.Errorf("the MCP server ran for an unloaded lazy tool (%d calls)", calls.Load())
	}
}

// A child of a profile with no tools.lazy gets the full, ungated definition: no tool_not_loaded,
// and the real schema. The child's call with no "word" is rejected by the real schema as
// validation_failed; a gate that did not check the real schema would have accepted it.
func TestChildOfAParentWithoutLazyToolsGetsTheFullMCPTool(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"read", "mcp:fixture:lookup"}}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{name, `{}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests", len(child))
	}
	history := child[len(child)-1].Text
	if strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the child was refused as a lazy tool:\n%s", history)
	}
	if !strings.Contains(history, "validation_failed") {
		t.Errorf("the child's call was not checked against the real schema:\n%s", history)
	}
	if calls.Load() != 0 {
		t.Errorf("the MCP server ran a call that the real schema should have refused (%d)", calls.Load())
	}
}

// tools.lazy with no catalog layout (no state folder) is a configuration error: the session
// has nowhere to keep the loaded set, so a load would fail closed.
func TestOpenLazySetWithoutAStateFolderIsAConfigError(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev", StateDir: "", ProfileTools: sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"read"}, Lazy: []string{"read"}, LazySummaryBytes: 256}}
	set, err := openLazySet(config, nil)
	if set != nil || err == nil || !strings.Contains(err.Error(), "catalog layout") {
		t.Fatalf("openLazySet with no state folder = %v, %v; want the catalog-layout error", set, err)
	}
	var configError *sessionfs.ConfigError
	if !errors.As(err, &configError) {
		t.Errorf("the error is not a configuration error: %T", err)
	}
}

// A profile without tools.lazy opens no set at all, so its tools array is what it was.
func TestOpenLazySetIsNilWithoutLazyEntries(t *testing.T) {
	set, err := openLazySet(sessionfs.SessionConfig{StateDir: t.TempDir()}, nil)
	if set != nil || err != nil {
		t.Fatalf("openLazySet with no lazy entries = %v, %v; want nil, nil", set, err)
	}
}

// A batch loads several lazy tools in one call. The file is written once, and the model's next
// call to each of them validates against the real schema.
func TestLazyBatchLoadsSeveralToolsInOneCall(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["read","` + name + `"]}`},
		{"read", `{"path":"notes.txt"}`},
		{name, `{"word":"answer"}`},
	}
	f, calls := lazyFixture(t, []string{"read", "mcp:fixture:lookup"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if strings.Contains(history, "tool_not_loaded") {
		t.Errorf("a batched tool was still refused after the load:\n%s", history)
	}
	if !strings.Contains(history, "the secret is 42") || calls.Load() != 1 {
		t.Errorf("the batched tools did not run (MCP calls %d):\n%s", calls.Load(), history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 2 {
		t.Errorf("loaded set = %v, want both tools", loaded)
	}
}

// An unknown name in a batch does not stop the valid ones, and gets its own section.
func TestLazyBatchWithAnUnknownNameLoadsTheRest(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{"names":["bogus","read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixture(t, []string{"read"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "bogus: not a lazy tool of this profile") {
		t.Errorf("the unknown name has no section:\n%s", history)
	}
	if !strings.Contains(history, "the secret is 42") {
		t.Errorf("the valid name did not load:\n%s", history)
	}
}

// A batch over tools.lazy_load_max is refused, and nothing is loaded. The limit counts matched tools,
// so the batch matches two lazy tools through one pattern.
func TestLazyBatchOverTheLimitLoadsNothing(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:fixture:*","read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixtureWithMax(t, []string{"read", "mcp:fixture:lookup"}, 1, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "matched 2 lazy tools, the limit is 1 (tools.lazy_load_max)") {
		t.Errorf("the batch was not refused by the limit:\n%s", history)
	}
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("read was loaded by a refused batch:\n%s", history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 0 {
		t.Errorf("loaded set = %v after a refused batch, want none", loaded)
	}
}

// A pattern expands to the lazy tools it selects: "mcp:fixture:*" loads the server's lazy tool,
// and the call that follows validates against its real schema.
func TestLazyPatternLoadsEveryLazyToolOfTheServer(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:fixture:*"]}`},
		{name, `{"word":"answer"}`},
	}
	f, calls := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if strings.Contains(history, "tool_not_loaded") || calls.Load() != 1 {
		t.Errorf("the pattern did not load the server's tool (MCP calls %d):\n%s", calls.Load(), history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 || loaded[0] != "mcp:fixture:lookup" {
		t.Errorf("loaded set = %v, want the one server tool", loaded)
	}
}

// A pattern that matches no lazy tool gets its own line; the other patterns still load, and an
// all-no-match batch writes nothing.
func TestLazyPatternWithNoMatchLoadsTheRest(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:nothere:*","read"]}`},
		{"read", `{"path":"notes.txt"}`},
	}
	f, _ := lazyFixture(t, []string{"read"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "no lazy tool matches mcp:nothere:*") {
		t.Errorf("the no-match pattern has no line:\n%s", history)
	}
	if !strings.Contains(history, "the secret is 42") {
		t.Errorf("the other pattern did not load:\n%s", history)
	}
}

// A tool the profile does not list as lazy is not loaded by "*": the profile lists only read, so
// "*" selects read alone, and the MCP lookup, which is not lazy, was never gated and still runs.
func TestLazyStarLoadsOnlyTheLazyToolsTheProfileListed(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["*"]}`},
		{name, `{"word":"answer"}`},
	}
	f, _ := lazyFixture(t, []string{"read"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if strings.Contains(history, "tool_not_loaded") {
		t.Errorf("a tool the profile did not list as lazy was gated:\n%s", history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 || loaded[0] != "native.read" {
		t.Errorf("loaded set = %v, want read alone", loaded)
	}
}

// Overlapping patterns load a tool once: the file lists it once, and its definition appears once,
// under the first section, while the later section says it is already listed above.
func TestLazyOverlappingPatternsShowAToolOnce(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:fixture:*","mcp:fixture:lookup"]}`},
	}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	if strings.Count(history, "inputSchema:") != 1 || !strings.Contains(history, "already listed above") {
		t.Errorf("the overlap was not shown once:\n%s", history)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 {
		t.Errorf("loaded set = %v, want one tool", loaded)
	}
}

// A profile that hides an MCP tool which a wildcard entry would match: the pattern loads only the
// tool the profile shows, the hidden tool never reaches the file or the output, and its call is
// refused as an unknown tool rather than as one not loaded.
func TestLazyPatternNeverLoadsAToolTheProfileHides(t *testing.T) {
	hidden := mcpToolNameFor("fixture", "hidden")
	lookup := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:fixture:*"]}`},
		{hidden, `{"word":"answer"}`},
		{lookup, `{"word":"answer"}`},
	}
	profileTools := map[string]any{"allow": []string{"load_tool", "mcp:fixture:*"}, "deny": []string{"mcp:fixture:hidden"}, "lazy": []string{"mcp:fixture:*"}}
	f, calls := lazyFixtureWithExtraTools(t, profileTools, []string{"hidden"}, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	loadResult := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if strings.Contains(loadResult, hidden) || strings.Contains(loadResult, "mcp:fixture:hidden") {
		t.Errorf("the pattern output names the hidden tool:\n%s", loadResult)
	}
	if !strings.Contains(loadResult, lookup) {
		t.Errorf("the visible lazy tool is missing from the pattern output:\n%s", loadResult)
	}
	hiddenResult := history[strings.Index(history, `"call_id":"call-step-2"`):strings.Index(history, `"call_id":"call-step-3"`)]
	if !strings.Contains(hiddenResult, "unknown_tool") || strings.Contains(hiddenResult, "tool_not_loaded") {
		t.Errorf("the hidden tool's call was not refused as unknown:\n%s", hiddenResult)
	}
	if calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1: the visible lookup only", calls.Load())
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 || loaded[0] != "mcp:fixture:lookup" {
		t.Errorf("loaded set = %v, want the visible lookup alone", loaded)
	}
}

// A server with more lazy tools than tools.lazy_load_max: the wildcard over the server is refused
// with nothing loaded, and the error names the limit and the match count. A narrower selection
// within the limit loads and writes the file.
func TestLazyPatternOverTheLimitOnALargeServerLoadsNothing(t *testing.T) {
	extra := make([]string, 0, 17)
	for index := 1; index <= 17; index++ {
		extra = append(extra, fmt.Sprintf("t%02d", index))
	}
	narrow := mcpToolNameFor("fixture", "t01")
	steps := []subagentStep{
		{"load_tool", `{"names":["mcp:fixture:*"]}`},
		{"load_tool", `{"names":["mcp:fixture:t01"]}`},
		{narrow, `{"word":"answer"}`},
	}
	profileTools := map[string]any{"allow": []string{"load_tool", "mcp:fixture:*"}, "lazy": []string{"mcp:fixture:*"}}
	f, calls := lazyFixtureWithExtraTools(t, profileTools, extra, steps)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	history := lastRequestText(t, parentRequests(f))
	refused := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if !strings.Contains(refused, "matched 18 lazy tools, the limit is 16 (tools.lazy_load_max)") {
		t.Errorf("the wildcard was not refused by the limit:\n%s", refused)
	}
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 1 || loaded[0] != "mcp:fixture:t01" {
		t.Errorf("loaded set = %v, want the narrow selection alone", loaded)
	}
	if calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1 for the narrow tool", calls.Load())
	}
}

// load_tool is not in a subagent child's catalog: the child is built from the parent's native tools
// before load_tool exists, so the child's call is refused as an unknown tool, and it cannot load
// lazy tools for the parent, whose state file is never written.
func TestChildCannotLoadLazyToolsForTheParent(t *testing.T) {
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "read", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{"load_tool", `{"names":["mcp:fixture:*"]}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests", len(child))
	}
	history := child[len(child)-1].Text
	if !strings.Contains(history, "unknown_tool") {
		t.Errorf("the child's load_tool call was not refused as unknown:\n%s", history)
	}
	// The parent's set is opened when its run is prepared, so the file exists with an empty loaded
	// list; the child's refused call must not have added to it.
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 0 {
		t.Errorf("the child loaded lazy tools for the parent: %v", loaded)
	}
	if calls.Load() != 0 {
		t.Errorf("the child reached the MCP server through load_tool's refusal (%d calls)", calls.Load())
	}
}
