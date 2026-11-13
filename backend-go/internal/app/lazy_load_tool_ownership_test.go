package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// The lazy list is load_tool's config (tools.native.load_tool.lazy in a toolset), not the profile's.
// These tests build the session the way a user does, with the list in a toolset file the profile names,
// and run it through the real loader, resolver and routes.

// loadToolOwnedFixture is a session whose own toolset carries loadTool as its native.load_tool config and
// whose profile has the tools block profileTools (nil: no block, an unrestricted profile). The MCP server
// has lookup and the extra tools; the extension web_search tool is enabled too.
func loadToolOwnedFixture(t *testing.T, loadTool, profileTools map[string]any, extra []string, steps []subagentStep) (subagentFixture, *atomic.Int32) {
	t.Helper()
	session := map[string]any{"__load_tool_config": loadTool}
	if profileTools != nil {
		session["__profile_extra"] = map[string]any{"tools": profileTools}
	}
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "__session": session, "__load_tool": true, "__web_search": &atomic.Int32{}}, false, nil, &mcpFixture{calls: calls, extra: extra})
	f.provider.parentSteps = steps
	return f, calls
}

// A profile with no tools block whose toolset lists a lazy MCP tool: the session loads, the model's tools
// array lacks the tool and shows load_tool, load_tool lists, describes and runs it, and the tools route
// reports it as lazy.
func TestUnrestrictedProfileRunsALazyListFromItsToolset(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{
		describeStep("mcp:fixture:lookup"),
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
	}
	f, calls := loadToolOwnedFixture(t, map[string]any{"lazy": []string{"mcp:fixture:lookup"}}, nil, nil, steps)
	eager, lazy := sessionToolRows(t, f)
	if slices.Contains(eager, name) || !slices.Contains(lazy, name) || !slices.Contains(eager, "load_tool") || !slices.Contains(eager, "skill") {
		t.Fatalf("the route lists %v and lazy %v, want load_tool and the native tools eager and %s lazy", eager, lazy, name)
	}
	requireCompleted(t, f)
	requests := parentRequests(f)
	requireStableTools(t, requests, name)
	if tools := requests[0].Tools; !slices.Contains(tools, "load_tool") || !slices.Contains(tools, "skill") || !slices.Contains(tools, "web_search") {
		t.Errorf("the model's tools = %v, want load_tool and the eager tools", tools)
	}
	history := lastRequestText(t, requests)
	if !strings.Contains(history, "name: "+name) || !strings.Contains(history, "MCP says answer is 99") || calls.Load() != 1 {
		t.Errorf("load_tool did not describe and run the lazy tool (MCP calls %d):\n%s", calls.Load(), history)
	}
	requireNoLazyFile(t, f)
}

// An unrestricted profile gets the same strict check as a restricted one: a lazy entry that names no tool
// the profile shows is a configuration error, not a tool that is quietly absent.
func TestUnrestrictedProfileRefusesALazyEntryThatNamesNoShownTool(t *testing.T) {
	f, _ := loadToolOwnedFixture(t, map[string]any{"lazy": []string{"mcp:fixture:lookup", "mcp:fixture:missing"}}, nil, nil, nil)
	status, body := f.sessionTools(t)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", status, body)
	}
	for _, want := range []string{"tools.native.load_tool.lazy[1]", "mcp:fixture:missing"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %s does not contain %q", body, want)
		}
	}
}

// A tools block with deny and no allow keeps every tool but the denied: the list loads and runs, a hidden
// tool stays out of a pattern, and a deny that hides a listed entry (or load_tool itself) is refused.
func TestDenyOnlyProfileRunsALazyListAndRefusesWhatItHides(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	hidden := mcpToolNameFor("fixture", "hidden")
	steps := []subagentStep{
		describeStep("mcp:fixture:*"),
		runStep("mcp:fixture:lookup", `{"word":"answer"}`),
	}
	profileTools := map[string]any{"deny": []string{"mcp:fixture:hidden"}}
	f, calls := loadToolOwnedFixture(t, map[string]any{"lazy": []string{"mcp:fixture:*"}}, profileTools, []string{"hidden"}, steps)
	requireCompleted(t, f)
	requests := parentRequests(f)
	requireStableTools(t, requests, name, hidden)
	if !slices.Contains(requests[0].Tools, "load_tool") {
		t.Errorf("the model's tools = %v, want load_tool", requests[0].Tools)
	}
	history := lastRequestText(t, requests)
	if !strings.Contains(history, "name: "+name) || strings.Contains(history, hidden) || calls.Load() != 1 {
		t.Errorf("the pattern described the hidden tool or the run missed (MCP calls %d):\n%s", calls.Load(), history)
	}

	for label, tc := range map[string]struct {
		deny []string
		want []string
	}{
		"deny hides a listed entry": {[]string{"mcp:fixture:lookup"}, []string{"tools.native.load_tool.lazy[0]", "mcp:fixture:lookup"}},
		"deny hides load_tool":      {[]string{"load_tool"}, []string{"tools.native.load_tool.lazy", `hides \"load_tool\"`, "tools.deny"}},
	} {
		f, _ := loadToolOwnedFixture(t, map[string]any{"lazy": []string{"mcp:fixture:lookup"}}, map[string]any{"deny": tc.deny}, nil, nil)
		status, body := f.sessionTools(t)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422; body %s", label, status, body)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: refusal %s does not contain %q", label, body, want)
			}
		}
	}
}

// One toolset with a lazy list, named by two profiles: the profile that shows every entry loads and runs
// them; the profile whose deny hides one entry is refused at its own catalog build, naming the key; the
// first profile is unaffected by the second's refusal.
func TestSharedToolsetLazyListIsCheckedForEachProfile(t *testing.T) {
	f := newProfileLazyFixture(t, nil, []string{"mcp:fixture:lookup", "mcp:fixture:extra01"}, false)
	strict := `{"models":["openai/target"],"model_default":"openai/target","toolset":"coding/target","tools":{"allow":["load_tool","mcp:fixture:*"],"deny":["mcp:fixture:extra01"]}}`
	writeFile(t, filepath.Join(f.layout.Root, "profiles", "review", "strict.json"), []byte(strict))
	// The caller's session scope names the third profile too, so the caller can message it.
	sessionFile := filepath.Join(f.layout.SessionsDir, "demo.json")
	session, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	scoped := strings.Replace(string(session), `"review/reviewer"`, `"review/reviewer", "review/strict"`, 1)
	if scoped == string(session) {
		t.Fatalf("the session file does not name review/reviewer:\n%s", session)
	}
	writeFile(t, sessionFile, []byte(scoped))

	message := func() {
		t.Helper()
		f.scripts["target-model"] = []string{functionCallItem("load_tool", `{"run":"mcp:fixture:lookup","arguments":{"word":"x"}}`)}
		if _, err := (profileMessenger{resolver: f.service.resolver}).Message(f.callerContext(t), "local:demo\x00dev/coder", "review/reviewer", "hi", 0, false); err != nil {
			t.Fatal(err)
		}
	}
	message()
	if f.calls.Load() != 1 {
		t.Fatalf("the first profile's run reached the MCP server %d times, want 1", f.calls.Load())
	}
	if _, err = f.service.resolver.ListTools(context.Background(), "local:demo", "review/reviewer"); err != nil {
		t.Fatalf("the first profile's tools: %v", err)
	}
	_, err = f.service.resolver.ListTools(context.Background(), "local:demo", "review/strict")
	if err == nil {
		t.Fatal("the profile whose deny hides a listed entry loaded")
	}
	for _, want := range []string{"tools.native.load_tool.lazy[1]", "mcp:fixture:extra01", "strict"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	// The refusal changed nothing for the first profile: its tools route still lists both entries as lazy.
	listed, err := f.service.resolver.ListTools(context.Background(), "local:demo", "review/reviewer")
	if err != nil {
		t.Fatalf("the first profile's tools after the other's refusal: %v", err)
	}
	var lazy []string
	for _, row := range listed.Tools {
		if row.Lazy {
			lazy = append(lazy, row.Name)
		}
	}
	slices.Sort(lazy)
	want := []string{mcpToolNameFor("fixture", "extra01"), mcpToolNameFor("fixture", "lookup")}
	slices.Sort(want)
	if !slices.Equal(lazy, want) {
		t.Errorf("the first profile's lazy tools = %v, want %v", lazy, want)
	}
}

// A profile that names two toolsets with a load_tool each uses the first one's list as a whole: the
// second's list names a tool the first does not, and that tool stays eager.
func TestFirstToolsetsLazyListIsTheOneInEffect(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	session := map[string]any{
		"__load_tool_config": map[string]any{"lazy": []string{"mcp:fixture:lookup"}},
		"__profile_extra":    map[string]any{"toolset": []string{"coding/default", "extra/second"}},
	}
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{
		"model": "child.json", "__session": session, "__load_tool": true, "__web_search": &atomic.Int32{},
		"__toolsets": map[string]any{"extra/second": map[string]any{"native": map[string]any{"load_tool": map[string]any{"lazy": []string{"web_search"}}}}},
	}, false, nil, &mcpFixture{calls: calls})
	eager, lazy := sessionToolRows(t, f)
	if !slices.Contains(lazy, name) || slices.Contains(lazy, "web_search") || !slices.Contains(eager, "web_search") {
		t.Fatalf("the route lists %v and lazy %v, want only %s lazy (the first toolset's list) and web_search eager", eager, lazy, name)
	}
}

// An agent's toolset with no load_tool of its own, under an owner that has a lazy list, loads (it used to be
// refused): the child gates the owner's lazy tool with the owner's set and runs it through the owner's list.
func TestAgentToolsetWithoutLoadToolFollowsTheOwnersLazyList(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	for label, tc := range map[string]struct {
		call      [2]string
		wantCalls int32
		wantText  string
	}{
		"a direct call is refused":      {[2]string{name, `{"word":"answer"}`}, 0, "lazy_tool_not_loaded"},
		"a load_tool run uses the list": {[2]string{"load_tool", `{"run":"mcp:fixture:lookup","arguments":{"word":"answer"}}`}, 1, "MCP says answer is 99"},
	} {
		t.Run(label, func(t *testing.T) {
			calls := &atomic.Int32{}
			f := buildSubagentFixtureWithMCP(t, map[string]any{
				"model": "child.json", "toolset": "extra/lookup", "tools": []string{"load_tool", "mcp:fixture:lookup"},
				"__session": profileSession(map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}), "__load_tool": true,
				"__toolsets": map[string]any{"extra/lookup": map[string]any{
					"mcp": map[string]any{"m/fixture": map[string]any{"deny_tools": []string{}}},
				}},
			}, false, nil, &mcpFixture{calls: calls})
			f.provider.childFirstCall = tc.call
			requireCompleted(t, f)
			child := f.provider.forModel("child-model")
			if len(child) < 2 {
				t.Fatalf("child made %d requests", len(child))
			}
			if history := child[len(child)-1].Text; !strings.Contains(history, tc.wantText) {
				t.Errorf("the child's history lacks %q:\n%s", tc.wantText, history)
			}
			if calls.Load() != tc.wantCalls {
				t.Errorf("the MCP server ran %d times, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}
