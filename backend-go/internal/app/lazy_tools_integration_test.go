package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return lazyFixtureWithProfile(t, map[string]any{"allow": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy}, steps)
}

// lazyFixtureWithMax is lazyFixture with tools.lazy_load_max set, for the batch limit.
func lazyFixtureWithMax(t *testing.T, lazy []string, max int, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	return lazyFixtureWithProfile(t, map[string]any{"allow": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy, "lazy_load_max": max}, steps)
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

// runStep is the scripted call load_tool run target with arguments (a JSON object).
func runStep(target, arguments string) subagentStep {
	return subagentStep{"load_tool", `{"run":"` + target + `","arguments":` + arguments + `}`}
}

// describeStep is the scripted call load_tool names.
func describeStep(names ...string) subagentStep {
	encoded, _ := json.Marshal(names)
	return subagentStep{"load_tool", `{"names":` + string(encoded) + `}`}
}

// requireStableTools fails unless every request carried the same tools array byte for byte, and
// none of them names a tool in absent: the array a provider caches does not change with the use of
// load_tool.
func requireStableTools(t *testing.T, requests []providerRequest, absent ...string) {
	t.Helper()
	if len(requests) < 2 {
		t.Fatalf("%d requests, want at least 2 to compare their tools arrays", len(requests))
	}
	first := requests[0].RawTools
	if len(first) == 0 {
		t.Fatal("the first request carried no tools")
	}
	for index, request := range requests {
		if !slices.Equal(request.RawTools, first) {
			t.Fatalf("the tools array of request %d differs from the first:\n%v\n%v", index, request.RawTools, first)
		}
		for _, name := range absent {
			if slices.Contains(request.Tools, name) {
				t.Fatalf("request %d lists the lazy tool %s: %v", index, name, request.Tools)
			}
		}
	}
}

// Nothing writes the lazy set now: the file keeps the empty loaded list it was opened with.
func requireNoLoads(t *testing.T, f subagentFixture) {
	t.Helper()
	if loaded := lazyFileLoaded(t, f.lazyFile()); len(loaded) != 0 {
		t.Errorf("lazy_tools.json holds %v, want no loads: load_tool describes and runs, it does not load", loaded)
	}
}

func requireCompleted(t *testing.T, f subagentFixture) {
	t.Helper()
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
}

func TestLazyToolIsRefusedWhenCalledAndRunsThroughLoadToolRun(t *testing.T) {
	steps := []subagentStep{
		{"skill", notesCall},
		runStep("skill", notesCall),
	}
	f, _ := lazyFixture(t, []string{"skill"}, steps)
	requireCompleted(t, f)
	// The last request holds the whole history: the refused direct call and the run.
	history := lastRequestText(t, parentRequests(f))
	refused := strings.Index(history, `"call_id":"call-step-1"`)
	ran := strings.Index(history, `"call_id":"call-step-2"`)
	if refused < 0 || ran < 0 {
		t.Fatalf("the history lacks one of the two calls:\n%s", history)
	}
	if !strings.Contains(history[refused:ran], "tool_not_loaded") || !strings.Contains(history[refused:ran], "load_tool with run") {
		t.Errorf("the direct call was not refused with the pointer to load_tool run:\n%s", history)
	}
	if strings.Contains(history[ran:], "tool_not_loaded") || !strings.Contains(history[ran:], "the secret is 42") {
		t.Errorf("the run did not run the tool:\n%s", history)
	}
	requireStableTools(t, parentRequests(f), "skill")
	requireNoLoads(t, f)
}

func TestLazyMCPToolIsRefusedWhenCalledAndRunsThroughLoadToolRun(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{name, `{"word":"answer"}`},
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
		runStep(name, `{"word":"answer"}`),
	}
	f, calls := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the direct MCP call was not refused:\n%s", history)
	}
	if calls.Load() != 2 || strings.Count(history, "MCP says answer is 99") != 2 {
		t.Errorf("the MCP server saw %d calls (want 2: a run by ID and a run by name); the history:\n%s", calls.Load(), history)
	}
	requireStableTools(t, parentRequests(f), name)
}

func TestLazyMCPToolIsNeverListedAndItsRealSchemaIsDescribedOnRequest(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep(name),
		runStep(name, `{"word":"answer"}`),
	}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	parent := parentRequests(f)
	if len(parent) < 3 {
		t.Fatalf("parent made %d requests, want at least 3", len(parent))
	}
	if !slices.Contains(parent[0].Tools, "load_tool") {
		t.Fatalf("tools = %v, want load_tool", parent[0].Tools)
	}
	requireStableTools(t, parent, name)
	// The describe result carries what the array no longer does: the real description and the server's inputSchema.
	history := lastRequestText(t, parent)
	describe := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	for _, want := range []string{"name: " + name, "id: mcp:fixture:lookup", "description: looks a word up", "inputSchema: ", "required", "additionalProperties"} {
		if !strings.Contains(describe, want) {
			t.Errorf("the describe result lacks %q:\n%s", want, describe)
		}
	}
	requireNoLoads(t, f)
}

// The tools array is the same bytes in every request of a run that lists, describes and runs a lazy tool:
// nothing load_tool does adds to it, so a provider's prompt cache over the tools is not rewritten.
func TestToolsArrayIsByteIdenticalAcrossListDescribeAndRun(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		{"load_tool", `{}`},
		describeStep("mcp:fixture:lookup"),
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
		{"load_tool", `{}`},
	}
	f, calls := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	parent := parentRequests(f)
	if len(parent) != 5 {
		t.Fatalf("parent made %d requests, want 5: one per scripted call and the answer", len(parent))
	}
	requireStableTools(t, parent, name)
	if calls.Load() != 1 || !strings.Contains(lastRequestText(t, parent), "MCP says answer is 99") {
		t.Errorf("the run did not reach the MCP server (%d calls)", calls.Load())
	}
}

// Decider ruling (a): the loaded check runs before the real schema. An unloaded lazy native call
// with arguments the real schema rejects is answered tool_not_loaded, not validation_failed, and
// its pre_tool hook still fires once.
func TestUnloadedLazyCallWithBadArgsIsRefusedBeforeValidation(t *testing.T) {
	steps := []subagentStep{{"skill", `{"name":5}`}}
	f, marker := lazyHookFixture(t, []string{"skill"}, steps, "skill")
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
	profileTools := map[string]any{"allow": []string{"skill", "load_tool"}, "lazy": []string{}}
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

func TestLazyToolsArrayIsTheSameAfterARestart(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
		{"skill", notesCall},
	}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	parent := parentRequests(f)
	requireStableTools(t, parent, name)
	before := parent[len(parent)-1].RawTools
	if slices.Index(parent[0].Tools, "skill") < 0 {
		t.Fatalf("tools before restart = %v, want skill", parent[0].Tools)
	}
	f.provider.mu.Lock()
	f.provider.parentSteps = []subagentStep{{"skill", notesCall}}
	f.provider.requests = nil
	f.provider.mu.Unlock()
	f.apiURL = f.restart(t, nil)
	f.run(t)
	after := parentRequests(f)
	if len(after) == 0 {
		t.Fatal("no request after restart")
	}
	if got := after[0].RawTools; !slices.Equal(got, before) {
		t.Errorf("tools after restart = %v, want the same bytes %v", got, before)
	}
}

func TestLazyToolRunsThroughLoadToolAfterARestart(t *testing.T) {
	steps := []subagentStep{runStep("skill", notesCall)}
	f, _ := lazyFixture(t, []string{"skill"}, steps)
	requireCompleted(t, f)
	f.provider.mu.Lock()
	// The history persists, so the call after the restart is the second scripted step.
	f.provider.parentSteps = []subagentStep{runStep("skill", notesCall), {"skill", notesCall}, runStep("skill", notesCall)}
	f.provider.requests = nil
	f.provider.mu.Unlock()
	f.apiURL = f.restart(t, nil)
	f.run(t)
	history := lastRequestText(t, parentRequests(f))
	after := strings.LastIndex(history, `"call_id":"call-step-3"`)
	if after < 0 {
		t.Fatalf("the run after restart was not made:\n%s", history)
	}
	if strings.Contains(history[after:], "tool_not_loaded") || !strings.Contains(history[after:], "the secret is 42") {
		t.Errorf("the run was refused or did not run after restart:\n%s", history[after:])
	}
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the direct call after restart was not refused:\n%s", history)
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
	profileTools := map[string]any{"allow": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "lazy": lazy}
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
	steps := []subagentStep{{"skill", notesCall}}
	f, marker := lazyHookFixture(t, []string{"skill"}, steps, "skill")
	f.run(t)
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one refused read, want 1", got)
	}
	if !strings.Contains(lastRequestText(t, parentRequests(f)), "tool_not_loaded") {
		t.Error("the call was not refused")
	}
}

func TestLazyNativeRunFiresTheTargetsHookOnce(t *testing.T) {
	steps := []subagentStep{runStep("skill", notesCall)}
	f, marker := lazyHookFixture(t, []string{"skill"}, steps, "skill")
	f.run(t)
	if got := fires(t, marker); got != 1 {
		t.Fatalf("pre_tool fired %d times for one run of the lazy skill, want 1", got)
	}
	if !strings.Contains(lastRequestText(t, parentRequests(f)), "the secret is 42") {
		t.Error("the run did not run the tool")
	}
}

// With a hook on every tool, a list fires load_tool's own hook once, and a run fires the target's once and
// not load_tool's: two calls, two firings.
func TestLoadToolHooksFireForAListAndTheTargetOnlyForARun(t *testing.T) {
	steps := []subagentStep{{"load_tool", `{}`}, runStep("skill", notesCall)}
	f, marker := lazyHookFixture(t, []string{"skill"}, steps, "*")
	f.run(t)
	if got := fires(t, marker); got != 2 {
		t.Fatalf("pre_tool fired %d times for a list and a run, want 2", got)
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
// A profile with tools.lazy whose deny hides load_tool could never load a lazy tool, and the profile
// would run with the lazy tools unreachable. The profile is refused, and the error names load_tool. An
// allow that leaves load_tool out does not hide it: tools.lazy shows load_tool.
func TestLazyProfileWhoseDenyHidesLoadToolIsAConfigError(t *testing.T) {
	registered := []tools.Definition{
		{ID: "native.skill", Source: "native", Name: "skill"},
		{ID: "native.load_tool", Source: "native", Name: "load_tool"},
	}
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev"}
	for name, settings := range map[string]sessionfs.ProfileToolSettings{
		"deny names it":           {Restricted: true, Deny: []string{"load_tool"}, Lazy: []string{"skill"}},
		"deny names it, allow it": {Restricted: true, HasAllow: true, Allow: []string{"load_tool"}, Deny: []string{"load_tool"}, Lazy: []string{"skill"}},
	} {
		err := checkLazyLoadToolShown(config, settings, registered)
		if err == nil || !strings.Contains(err.Error(), "load_tool") || !strings.Contains(err.Error(), "tools.lazy") || !strings.Contains(err.Error(), "tools.deny") {
			t.Errorf("%s: err = %v, want the refusal naming tools.lazy and load_tool", name, err)
		}
	}
	shown := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"skill", "load_tool"}, Lazy: []string{"skill"}}
	if err := checkLazyLoadToolShown(config, shown, registered); err != nil {
		t.Errorf("allow that shows load_tool: %v", err)
	}
	none := sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"skill"}}
	if err := checkLazyLoadToolShown(config, none, registered); err != nil {
		t.Errorf("no tools.lazy: %v", err)
	}
	// A deny that names another tool, or a deny-only list, leaves load_tool shown, so the profile loads.
	for name, valid := range map[string]sessionfs.ProfileToolSettings{
		"deny names another tool": {Restricted: true, Deny: []string{"mcp:fixture:hidden"}, Lazy: []string{"skill"}},
		"allow with a wildcard":   {Restricted: true, HasAllow: true, Allow: []string{"skill", "mcp:fixture:*", "load_tool"}, Lazy: []string{"skill"}},
		"allow omits load_tool":   {Restricted: true, HasAllow: true, Allow: []string{"skill"}, Lazy: []string{"skill"}},
		"empty allow":             {Restricted: true, HasAllow: true, Allow: []string{}, Lazy: []string{"skill"}},
	} {
		if err := checkLazyLoadToolShown(config, valid, registered); err != nil {
			t.Errorf("%s: a valid profile was refused: %v", name, err)
		}
	}
}

// A lazy entry the lazy pass can never bind is a configuration error too. load_tool is never bound, so a
// lazy entry that names it would be accepted and do nothing: the tool would stay in the array. The late
// transcript, profile, profile_message and subagent tools are bound, not refused.
func TestLazyEntryNamingAToolTheLazyPassCannotBindIsAConfigError(t *testing.T) {
	registered := []tools.Definition{
		{ID: "native.skill", Source: "native", Name: "skill"},
		{ID: "native.subagent", Source: "native", Name: "subagent"},
		{ID: "native.load_tool", Source: "native", Name: "load_tool"},
	}
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev"}
	for _, name := range []string{"load_tool"} {
		settings := sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{name}}
		err := checkLazyEntriesVisible(config, settings, registered)
		if err == nil || !strings.Contains(err.Error(), `tools.lazy[0] lists "`+name+`"`) || !strings.Contains(err.Error(), "cannot be lazy") {
			t.Errorf("%s: err = %v, want the lazy-entry refusal naming it", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), `"subagent"`) || strings.Contains(err.Error(), `"profile_message"`) || strings.Contains(err.Error(), `"transcript"`) || strings.Contains(err.Error(), `"profile"`)) {
			t.Errorf("%s: the refusal names a tool that can be lazy: %v", name, err)
		}
	}
	// profile_message and subagent are bound by the late pass, so a lazy entry naming either is accepted.
	for _, name := range []string{"profile_message", "subagent"} {
		accepted := sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{name}}
		if err := checkLazyEntriesVisible(config, accepted, []tools.Definition{{ID: "native." + name, Source: "native", Name: name}}); err != nil {
			t.Errorf("%s: lazy entry refused: %v", name, err)
		}
	}
}

func TestLazyEntryTheProfileHidesIsAConfigError(t *testing.T) {
	registered := []tools.Definition{
		{ID: "native.skill", Source: "native", Name: "skill"},
		{ID: "native.exec", Source: "native", Name: "exec"},
	}
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev"}
	for name, tc := range map[string]struct {
		settings sessionfs.ProfileToolSettings
		want     string
	}{
		"denied":          {sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{"skill"}, Deny: []string{"skill"}}, `tools.lazy[0] lists "skill"`},
		"denied, allowed": {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"skill"}, Lazy: []string{"skill"}, Deny: []string{"skill"}}, `tools.lazy[0] lists "skill"`},
		"outside allow":   {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"exec"}, Lazy: []string{"skill"}}, ""},
		"empty allow":     {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{}, Lazy: []string{"skill"}}, ""},
		"visible":         {sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"skill"}, Lazy: []string{"skill"}}, ""},
		"no allow, shown": {sessionfs.ProfileToolSettings{Restricted: true, Lazy: []string{"exec"}}, ""},
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
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
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
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "mcp:fixture:lookup"}}, false, nil, &mcpFixture{calls: calls})
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
	config := sessionfs.SessionConfig{SettingPath: "s.json", Profile: "dev", StateDir: "", ProfileTools: sessionfs.ProfileToolSettings{Restricted: true, HasAllow: true, Allow: []string{"skill"}, Lazy: []string{"skill"}, LazySummaryBytes: 256}}
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

func TestLazyBatchDescribesSeveralToolsInOneCall(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep("skill", name),
		runStep("skill", notesCall),
		runStep(name, `{"word":"answer"}`),
	}
	f, calls := lazyFixture(t, []string{"skill", "mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "name: skill") || !strings.Contains(history, "name: "+name) {
		t.Errorf("the batch did not describe both tools:\n%s", history)
	}
	if !strings.Contains(history, "the secret is 42") || calls.Load() != 1 {
		t.Errorf("the described tools did not run (MCP calls %d):\n%s", calls.Load(), history)
	}
	requireStableTools(t, parentRequests(f), "skill", name)
	requireNoLoads(t, f)
}

func TestLazyBatchWithAnUnknownNameDescribesTheRest(t *testing.T) {
	steps := []subagentStep{describeStep("bogus", "skill")}
	f, _ := lazyFixture(t, []string{"skill"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "bogus: not a lazy tool of this profile") {
		t.Errorf("the unknown name has no section:\n%s", history)
	}
	if !strings.Contains(history, "name: skill") {
		t.Errorf("the valid name was not described:\n%s", history)
	}
}

func TestLazyBatchOverTheLimitDescribesNothing(t *testing.T) {
	steps := []subagentStep{
		describeStep("mcp:fixture:*", "skill"),
		{"skill", notesCall},
	}
	f, _ := lazyFixtureWithMax(t, []string{"skill", "mcp:fixture:lookup"}, 1, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "matched 2 lazy tools, the limit is 1 (tools.lazy_load_max)") {
		t.Errorf("the batch was not refused by the limit:\n%s", history)
	}
	if strings.Contains(history, "name: skill") {
		t.Errorf("a refused batch described a tool:\n%s", history)
	}
	if !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("a direct call after a refused batch was not refused:\n%s", history)
	}
	requireNoLoads(t, f)
}

func TestLazyPatternDescribesEveryLazyToolOfTheServer(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep("mcp:fixture:*"),
		runStep(name, `{"word":"answer"}`),
	}
	f, calls := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "name: "+name) || calls.Load() != 1 {
		t.Errorf("the pattern did not describe the server's tool, or the run failed (MCP calls %d):\n%s", calls.Load(), history)
	}
	requireNoLoads(t, f)
}

func TestLazyPatternWithNoMatchDescribesTheRest(t *testing.T) {
	steps := []subagentStep{describeStep("mcp:nothere:*", "skill")}
	f, _ := lazyFixture(t, []string{"skill"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if !strings.Contains(history, "no lazy tool matches mcp:nothere:*") {
		t.Errorf("the no-match pattern has no line:\n%s", history)
	}
	if !strings.Contains(history, "name: skill") {
		t.Errorf("the other pattern was not described:\n%s", history)
	}
}

func TestLazyStarDescribesOnlyTheLazyToolsTheProfileListed(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep("*"),
		{name, `{"word":"answer"}`},
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
	}
	f, calls := lazyFixture(t, []string{"skill"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	describe := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if !strings.Contains(describe, "name: skill") || strings.Contains(describe, name) {
		t.Errorf("* did not describe read alone:\n%s", describe)
	}
	direct := history[strings.Index(history, `"call_id":"call-step-2"`):strings.Index(history, `"call_id":"call-step-3"`)]
	if strings.Contains(direct, "tool_not_loaded") || calls.Load() != 1 {
		t.Errorf("a tool the profile did not list as lazy was gated (MCP calls %d):\n%s", calls.Load(), direct)
	}
	if run := history[strings.Index(history, `"call_id":"call-step-3"`):]; !strings.Contains(run, "unknown_lazy_tool") {
		t.Errorf("a run of a tool that is not lazy was not refused:\n%s", run)
	}
}

func TestLazyOverlappingPatternsDescribeAToolOnce(t *testing.T) {
	steps := []subagentStep{describeStep("mcp:fixture:*", "mcp:fixture:lookup")}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	if strings.Count(history, "name: "+mcpToolNameFor("fixture", "lookup")) != 1 || !strings.Contains(history, "already described above") || strings.Count(history, "inputSchema:") != 1 {
		t.Errorf("the overlap was not described once:\n%s", history)
	}
}

func TestLazyPatternNeverDescribesOrRunsAToolTheProfileHides(t *testing.T) {
	hidden := mcpToolNameFor("fixture", "hidden")
	lookup := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep("mcp:fixture:*"),
		{hidden, `{"word":"answer"}`},
		runStep("mcp:fixture:hidden", `{"word":"answer"}`),
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
	}
	profileTools := map[string]any{"allow": []string{"load_tool", "mcp:fixture:*"}, "deny": []string{"mcp:fixture:hidden"}, "lazy": []string{"mcp:fixture:*"}}
	f, calls := lazyFixtureWithExtraTools(t, profileTools, []string{"hidden"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	describe := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if strings.Contains(describe, hidden) || strings.Contains(describe, "mcp:fixture:hidden") {
		t.Errorf("the pattern output names the hidden tool:\n%s", describe)
	}
	if !strings.Contains(describe, lookup) {
		t.Errorf("the visible lazy tool is missing from the pattern output:\n%s", describe)
	}
	hiddenResult := history[strings.Index(history, `"call_id":"call-step-2"`):strings.Index(history, `"call_id":"call-step-3"`)]
	if !strings.Contains(hiddenResult, "unknown_tool") || strings.Contains(hiddenResult, "tool_not_loaded") {
		t.Errorf("the hidden tool's call was not refused as unknown:\n%s", hiddenResult)
	}
	hiddenRun := history[strings.Index(history, `"call_id":"call-step-3"`):strings.Index(history, `"call_id":"call-step-4"`)]
	if !strings.Contains(hiddenRun, "unknown_lazy_tool") {
		t.Errorf("the run of the hidden tool was not refused:\n%s", hiddenRun)
	}
	if calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1: the visible lookup only", calls.Load())
	}
}

func TestLazyPatternOverTheLimitOnALargeServerDescribesNothing(t *testing.T) {
	extra := make([]string, 0, 17)
	for index := 1; index <= 17; index++ {
		extra = append(extra, fmt.Sprintf("t%02d", index))
	}
	narrow := mcpToolNameFor("fixture", "t01")
	steps := []subagentStep{
		describeStep("mcp:fixture:*"),
		describeStep("mcp:fixture:t01"),
		runStep("mcp:fixture:t01", `{"word":"answer"}`),
	}
	profileTools := map[string]any{"allow": []string{"load_tool", "mcp:fixture:*"}, "lazy": []string{"mcp:fixture:*"}}
	f, calls := lazyFixtureWithExtraTools(t, profileTools, extra, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	refused := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if !strings.Contains(refused, "matched 18 lazy tools, the limit is 16 (tools.lazy_load_max)") {
		t.Errorf("the wildcard was not refused by the limit:\n%s", refused)
	}
	described := history[strings.Index(history, `"call_id":"call-step-2"`):strings.Index(history, `"call_id":"call-step-3"`)]
	if !strings.Contains(described, "name: "+narrow) {
		t.Errorf("the narrow selection was not described:\n%s", described)
	}
	if calls.Load() != 1 {
		t.Errorf("MCP calls = %d, want 1 for the narrow tool", calls.Load())
	}
	requireNoLoads(t, f)
}

// load_tool is not in a subagent child's catalog: the child is built from the parent's native tools
// before load_tool exists, so the child's call is refused as an unknown tool, and it cannot load
// lazy tools for the parent, whose state file is never written.
func TestChildCannotLoadLazyToolsForTheParent(t *testing.T) {
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
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

// The resolver gives the owning profile's lazy set to the subagent tool (SetLazy) before it binds the MCP
// tools into the children (BindToolsets). The order is pinned here: a profile with a lazy subagent whose child
// lists a lazy MCP tool must refuse that child's call until the owner loads it. If BindToolsets ran first, the
// child's copy would be built with no set and would run, so this test would fail.
func TestLazySubagentProfileGivesTheChildItsLazySetBeforeBindingMCP(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	calls := &atomic.Int32{}
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"subagent", "mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "mcp:fixture:lookup"}, "__session": profileSession(profileTools), "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	// The parent runs the lazy subagent through load_tool: a call that names subagent is refused.
	f.provider.parentSteps = []subagentStep{runStep("subagent", `{"task":"look it up","agent":"explore"}`)}
	f.provider.childFirstCall = [2]string{name, `{}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests", len(child))
	}
	if history := child[len(child)-1].Text; !strings.Contains(history, "tool_not_loaded") {
		t.Errorf("the child's call to the unloaded lazy MCP tool was not refused:\n%s", history)
	}
	if calls.Load() != 0 {
		t.Errorf("the MCP server ran for an unloaded lazy MCP tool (%d calls)", calls.Load())
	}
}

// The resolver gives the subagent tool its lazy set (SetLazy) before it binds the MCP tools (BindToolsets). A
// child copy is gated when BindToolsets builds it, so a reversed order builds every child copy with no set.
// This test fails under that order, and the message names the order, so a reversal is not read as a mystery.
// It reads resolver.go as text, so it breaks on a rename or reformat of those two calls; then update the names here.
// The real check is the behavioural test TestLazySubagentProfileGivesTheChildItsLazySetBeforeBindingMCP.
func TestResolverGivesTheSubagentItsLazySetBeforeItBindsMCPTools(t *testing.T) {
	src, err := os.ReadFile("resolver.go")
	if err != nil {
		t.Fatal(err)
	}
	setLazy := strings.Index(string(src), "subagentMCP.SetLazy(lazy, childLoadTool)")
	bind := strings.Index(string(src), "subagentMCP.BindToolsets(mcpTools, toolsetMCP)")
	if setLazy < 0 || bind < 0 {
		t.Fatal("resolver.go no longer calls SetLazy and BindToolsets by these names; update this pin")
	}
	if setLazy > bind {
		t.Fatal("resolver.go calls subagentMCP.BindToolsets before SetLazy: each child's MCP copy is built with no lazy set, so the child's lazy MCP tool is not gated")
	}
}
