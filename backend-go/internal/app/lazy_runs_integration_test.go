package app

import (
	"strings"
	"testing"
)

// runsStep is the scripted load_tool call with runs: items are the JSON objects of the list.
func runsStep(items ...string) subagentStep {
	return subagentStep{"load_tool", `{"runs":[` + strings.Join(items, ",") + `]}`}
}

// One load_tool call runs an extension tool and an MCP tool twice, with a name that is no lazy tool
// among them. The model gets one result with every output in the order sent, the next request
// replays it, the MCP server saw each call, and the tools array never changed.
func TestLazyRunsRunSeveralToolsInOneCall(t *testing.T) {
	name := mcpToolNameFor("fixture", "lookup")
	steps := []subagentStep{runsStep(
		`{"run":"web_search","arguments":`+searchCall+`}`,
		`{"run":"mcp:fixture:lookup","arguments":{"word":"answer"}}`,
		`{"run":"bogus"}`,
		`{"run":"`+name+`","arguments":{"word":"answer"}}`,
	)}
	f, calls := lazyFixture(t, []string{"web_search", "mcp:fixture:lookup"}, steps)
	requireCompleted(t, f)
	history := lastRequestText(t, parentRequests(f))
	at := 0
	for _, want := range []string{
		"--- run 1 of 4: web_search ---", "Lazy search hit",
		"--- run 2 of 4: mcp:fixture:lookup ---", "MCP says answer is 99",
		"--- run 3 of 4: bogus ---", "unknown_lazy_tool",
		"--- run 4 of 4: " + name + " ---", "MCP says answer is 99",
	} {
		found := strings.Index(history[at:], want)
		if found < 0 {
			t.Fatalf("%q is missing, or out of order, in the replayed history:\n%s", want, history)
		}
		at += found + len(want)
	}
	if calls.Load() != 2 || f.searchCalls.Load() != 1 {
		t.Errorf("the MCP server saw %d calls and the search API %d, want 2 and 1", calls.Load(), f.searchCalls.Load())
	}
	if strings.Contains(history, "lazy_tool_not_loaded") {
		t.Errorf("an item of the batch was refused as a direct call:\n%s", history)
	}
	requireStableTools(t, parentRequests(f), "web_search", name)
	requireNoLazyFile(t, f)
}

// A batch over tools.lazy_load_max runs nothing, and one that mixes runs with another key is refused
// before any tool starts; the next request replays the refusal.
func TestLazyRunsRefusedBatchesRunNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		step subagentStep
		want string
	}{
		"over the limit": {runsStep(`{"run":"mcp:fixture:lookup","arguments":{"word":"a"}}`, `{"run":"mcp:fixture:lookup","arguments":{"word":"b"}}`), "runs lists 2 tools, the limit is 1 (tools.lazy_load_max)"},
		"with names":     {subagentStep{"load_tool", `{"runs":[{"run":"mcp:fixture:lookup","arguments":{"word":"a"}}],"names":["web_search"]}`}, "runs cannot be combined"},
		"with run":       {subagentStep{"load_tool", `{"runs":[{"run":"mcp:fixture:lookup","arguments":{"word":"a"}}],"run":"web_search"}`}, "runs cannot be combined"},
	} {
		t.Run(name, func(t *testing.T) {
			f, calls := lazyFixtureWithMax(t, []string{"web_search", "mcp:fixture:lookup"}, 1, []subagentStep{tc.step})
			requireCompleted(t, f)
			history := lastRequestText(t, parentRequests(f))
			if !strings.Contains(history, tc.want) {
				t.Errorf("the refusal %q is not in the history:\n%s", tc.want, history)
			}
			if calls.Load() != 0 || f.searchCalls.Load() != 0 {
				t.Errorf("a refused batch ran tools (MCP %d, search %d)", calls.Load(), f.searchCalls.Load())
			}
		})
	}
}
