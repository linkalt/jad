package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// sessionToolNames returns the names the session's tools endpoint lists for the profile.
func sessionToolNames(t *testing.T, f subagentFixture) []string {
	t.Helper()
	status, body := f.sessionTools(t)
	if status != 200 {
		t.Fatalf("tools status = %d, body %s", status, body)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("tools body %s: %v", body, err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// load_tool is not added by the app: a profile without tools.native.load_tool has no load_tool. Its
// tools.lazy is refused at load for the missing key, so the profile never runs without load_tool.
func TestLoadToolIsAbsentWithoutItsNativeKey(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(map[string]any{"allow": []string{"skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}})}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a profile with tools.lazy and no load_tool key was not refused: %s", body)
	}
	if !strings.Contains(body, "tools.native.load_tool is not enabled") {
		t.Fatalf("refusal = %s, want the missing load_tool key named", body)
	}
}

// A tools.lazy entry is allowed without an allow entry, and so is load_tool while the profile has lazy
// tools: an allow that names neither still lists load_tool, and the lazy web_search is never listed, before or
// after load_tool runs it.
func TestLazyToolsNeedNoAllowEntry(t *testing.T) {
	steps := []subagentStep{runStep("web_search", searchCall)}
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"allow": []string{"mcp:fixture:lookup"}, "lazy": []string{"web_search"}}, nil, steps)
	status, body := f.sessionTools(t)
	if status != http.StatusOK {
		t.Fatalf("a lazy profile whose allow names neither read nor load_tool was refused: %d %s", status, body)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, "web_search") || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a run the route lists %v, want load_tool and no lazy web_search", names)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, "web_search") || !slices.Contains(names, "load_tool") {
		t.Fatalf("after a run the route lists %v, want load_tool and still no lazy web_search", names)
	}
	if got := lastRequestText(t, parentRequests(f)); !strings.Contains(got, "Lazy search hit") {
		t.Fatalf("load_tool did not run the lazy web_search:\n%s", got)
	}
}

// deny still wins over tools.lazy: a lazy profile whose deny names load_tool is refused, since load_tool
// is the only way to load its lazy tools.
func TestLoadToolHiddenByDenyRefusesALazyProfile(t *testing.T) {
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"deny": []string{"load_tool"}, "lazy": []string{"web_search"}}, nil, nil)
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a lazy profile whose deny hides load_tool was not refused: %s", body)
	}
	if !strings.Contains(body, `hides \"load_tool\"`) || !strings.Contains(body, "tools.deny") {
		t.Fatalf("refusal = %s, want it to name the hidden load_tool and tools.deny", body)
	}
}

// With tools.native.load_tool enabled and tools.lazy set, the session lists load_tool.
func TestLoadToolIsListedWhenEnabled(t *testing.T) {
	f, _ := lazyFixture(t, []string{"web_search"}, nil)
	if !slices.Contains(sessionToolNames(t, f), "load_tool") {
		t.Fatal("load_tool is not listed with tools.native.load_tool enabled")
	}
}

func TestLazyRouteNeverListsALazyExtensionTool(t *testing.T) {
	steps := []subagentStep{runStep("web_search", searchCall)}
	f, _ := lazyFixture(t, []string{"web_search"}, steps)
	if names := sessionToolNames(t, f); slices.Contains(names, "web_search") || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a run the route lists %v, want load_tool and no lazy web_search", names)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, "web_search") {
		t.Fatalf("after a run the route lists %v, want no lazy web_search", names)
	}
}

func TestLazyRouteNeverListsALazyMCPTool(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{runStep("mcp:fixture:lookup", `{"word":"answer"}`)}
	f, _ := lazyFixture(t, []string{"mcp:fixture:lookup"}, steps)
	if names := sessionToolNames(t, f); slices.Contains(names, name) || !slices.Contains(names, "load_tool") {
		t.Fatalf("before a run the route lists %v, want load_tool and no lazy %s", names, name)
	}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, name) {
		t.Fatalf("after a run the route lists %v, want no lazy %s", names, name)
	}
}

// tools.lazy without tools.native.load_tool refuses the session at load, and the error names both.
func TestLazyWithoutLoadToolRefusesTheSession(t *testing.T) {
	profileTools := map[string]any{"allow": []string{"skill", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": profileSession(profileTools)}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	if status == 200 {
		t.Fatalf("tools status = 200 with lazy and no load_tool; body %s", body)
	}
	for _, want := range []string{"tools.lazy", "tools.native.load_tool"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %s does not name %s", body, want)
		}
	}
}

// An extension tool is lazy like an MCP tool: it is never in the tools array, load_tool lists it, describes it
// (id, description, inputSchema) and runs it, the extension's own server answers, the array is the same bytes
// in every request, and no state file is written.
func TestLazyExtensionToolIsListedDescribedAndRunThroughLoadTool(t *testing.T) {
	steps := []subagentStep{
		{"load_tool", `{}`},
		describeStep("web_search"),
		runStep("web_search", searchCall),
		{"web_search", searchCall},
	}
	f, _ := lazyFixture(t, []string{"web_search"}, steps)
	requireCompleted(t, f)
	parent := parentRequests(f)
	requireStableTools(t, parent, "web_search")
	history := lastRequestText(t, parent)
	for _, want := range []string{"web_search — Search the web", "id: extension.web_search", "inputSchema: ", "Lazy search hit"} {
		if !strings.Contains(history, want) {
			t.Errorf("the history lacks %q:\n%s", want, history)
		}
	}
	if f.searchCalls.Load() != 1 {
		t.Errorf("the search server saw %d requests, want 1: the run (the direct call is refused)", f.searchCalls.Load())
	}
	if !strings.Contains(history[strings.LastIndex(history, `"call_id":"call-step-4"`):], "tool_not_loaded") {
		t.Errorf("the direct call of the lazy extension tool was not refused:\n%s", history)
	}
	if names := sessionToolNames(t, f); slices.Contains(names, "web_search") {
		t.Errorf("the tools route lists the lazy extension tool: %v", names)
	}
	requireNoLazyFile(t, f)
}

// A native tool is always listed and called by its own name, whatever the profile makes lazy: skill stays in
// the tools array and a direct call runs it, beside a lazy extension tool and a lazy MCP tool.
func TestNativeToolStaysEagerBesideLazyExtensionAndMCPTools(t *testing.T) {
	steps := []subagentStep{{"skill", notesCall}, describeStep("*")}
	f, _ := lazyFixture(t, []string{"web_search", "mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	parent := parentRequests(f)
	requireStableTools(t, parent, "web_search", mcpToolNameFor("fixture", "lookup"))
	if !slices.Contains(parent[0].Tools, "skill") || !slices.Contains(parent[0].Tools, "load_tool") {
		t.Fatalf("tools = %v, want the native skill and load_tool listed", parent[0].Tools)
	}
	history := lastRequestText(t, parent)
	call1 := history[strings.Index(history, `"call_id":"call-step-1"`):strings.Index(history, `"call_id":"call-step-2"`)]
	if strings.Contains(call1, "tool_not_loaded") || !strings.Contains(call1, "the secret is 42") {
		t.Errorf("the direct call of the native skill did not run:\n%s", call1)
	}
	// "*" describes the lazy tools only: the extension and the MCP tool, never a native one.
	describe := history[strings.Index(history, `"call_id":"call-step-2"`):]
	if !strings.Contains(describe, "name: web_search") || !strings.Contains(describe, "name: "+mcpToolNameFor("fixture", "lookup")) || strings.Contains(describe, "name: skill") || strings.Contains(describe, "name: load_tool") {
		t.Errorf("* did not describe exactly the lazy extension and MCP tools:\n%s", describe)
	}
}

// A profile whose tools.lazy names a native tool does not load: the error names the entry and the kinds that
// can be lazy.
func TestLazyEntryNamingANativeToolRefusesTheSession(t *testing.T) {
	f, _ := lazyFixtureWithExtraTools(t, map[string]any{"allow": []string{"skill", "load_tool"}, "lazy": []string{"skill"}}, nil, nil)
	status, body := f.sessionTools(t)
	if status == http.StatusOK {
		t.Fatalf("a profile with a native lazy entry loaded: %s", body)
	}
	for _, want := range []string{`tools.lazy[0]`, `\"skill\" is a native tool and cannot be lazy`, "decide_systemone, web_search"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %s does not contain %q", body, want)
		}
	}
}
