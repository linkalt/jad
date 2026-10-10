package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/agent-harness/backend-go/internal/tools"
)

// profileSession is the extra session setting of a profile with a tools block.
func profileSession(tools map[string]any) map[string]any {
	return map[string]any{"paths": map[string]any{
		"active_profile": "lean",
		"profiles":       map[string]any{"lean": map[string]any{"tools": tools}},
	}}
}

// A profile that shows its model only the subagent tool: the child still reads a file,
// although `read` is not among the tools the parent was sent.
func TestProfileToolsHideAToolFromTheParentButNotFromAChild(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.",
		"__session": profileSession(map[string]any{"allow": []string{"subagent"}}),
	})
	f.run(t)
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) == 0 || len(child) == 0 {
		t.Fatalf("requests: parent %d, child %d", len(parent), len(child))
	}
	if got := strings.Join(parent[0].Tools, ","); got != "subagent" {
		t.Errorf("parent tools = %q, want only subagent", got)
	}
	if got := strings.Join(child[0].Tools, ","); got != "read" {
		t.Errorf("child tools = %q, want read, which the profile hides from the parent", got)
	}
	if !strings.Contains(child[len(child)-1].Text, "function_call_output") {
		t.Errorf("the child's read call did not run:\n%s", child[len(child)-1].Text)
	}
}

// Without a tools block the parent sees every registered tool, as before.
func TestProfileWithoutToolsBlockShowsEveryRegisteredTool(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Reads files."})
	f.run(t)
	parent := f.provider.forModel("parent-model")
	if len(parent) == 0 {
		t.Fatal("the parent made no request")
	}
	if got := strings.Join(parent[0].Tools, ","); got != "read,subagent" {
		t.Errorf("parent tools = %q, want read,subagent", got)
	}
}

// An empty allow list hides every tool: the parent is sent none.
func TestProfileToolsEmptyAllowSendsNoTools(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.",
		"__session": profileSession(map[string]any{"allow": []string{}}),
	})
	f.run(t)
	parent := f.provider.forModel("parent-model")
	if len(parent) == 0 || len(parent[0].Tools) != 0 {
		t.Fatalf("parent requests %+v, want one with no tools", parent)
	}
}

// runStatus posts a run and returns the status and body, for a run that must be refused.
func (f subagentFixture) runStatus(t *testing.T, input string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(`{"input":"`+input+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, errorText(raw)
}

// errorText is the "error" field of a JSON error body, unescaped; any other body is
// returned as it is.
func errorText(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return body.Error
	}
	return string(raw)
}

// A tool name that does not exist is refused when the settings load (422 on the run). A
// real tool name the session did not enable, or an MCP tool that is not loaded, is found
// when the catalog is built: the run ends with the generic error event, as it does for
// an unreachable MCP server, and the tools route gives the full message. Neither is a
// tool that is quietly missing.
func TestProfileToolsEntryThatIsNotRegisteredIsAConfigError(t *testing.T) {
	for name, tc := range map[string]struct {
		entry, want string
		atLoad      bool
	}{
		"misspelled":            {"raed", `allow[0] "raed" is not a native tool; use one of bash, decide`, true},
		"not enabled":           {"bash", `lists "bash", which is not a registered tool (registered: read, subagent)`, false},
		"mcp not loaded":        {"mcp:ghost:lookup", `lists "mcp:ghost:lookup", which is not a registered tool`, false},
		"mcp server not listed": {"mcp:ghost:*", `lists "mcp:ghost:*", which is not a registered tool`, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSubagentFixture(t, map[string]any{
				"model": "child.json", "description": "Reads files.",
				"__session": profileSession(map[string]any{"allow": []string{tc.entry}}),
			})
			status, body := f.runStatus(t, "hello")
			if tc.atLoad {
				if status != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
					t.Fatalf("run: status %d, body %s", status, body)
				}
			} else if status != http.StatusOK || !strings.Contains(body, "event: error") {
				t.Fatalf("run: status %d, want 200 with an error event, body %s", status, body)
			}
			if len(f.provider.forModel("parent-model")) != 0 {
				t.Errorf("the parent must not reach the provider with a bad allow list")
			}
			status, body = f.sessionTools(t)
			body = errorText([]byte(body))
			if status != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
				t.Fatalf("tools route: status %d, body %s", status, body)
			}
		})
	}
}

// The tools route lists what the profile shows and, apart, what it hides.
func TestToolsRouteListsHiddenToolsApart(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.",
		"__session": profileSession(map[string]any{"allow": []string{"subagent"}}),
	})
	var body struct {
		Tools  []struct{ Name string } `json:"tools"`
		Hidden []struct{ Name string } `json:"hidden"`
	}
	status, raw := f.sessionTools(t)
	if status != http.StatusOK {
		t.Fatalf("tools route status %d: %s", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "subagent" || len(body.Hidden) != 1 || body.Hidden[0].Name != "read" {
		t.Fatalf("tools route = %+v", body)
	}
	plain := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Reads files."})
	var all map[string]any
	status, raw = plain.sessionTools(t)
	if status != http.StatusOK {
		t.Fatalf("tools route status %d: %s", status, raw)
	}
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		t.Fatal(err)
	}
	if _, has := all["hidden"]; has {
		t.Errorf("a profile without a tools block must not carry a hidden key: %v", all)
	}
}

func mcpNames(definitions []struct{ ID, Source, Name string }) string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Source+":"+definition.ID)
	}
	return strings.Join(names, ",")
}

// An MCP tool is named in the allow list by id, or the whole server by "mcp:<server>:*".
// A native tool the list leaves out is hidden, and the MCP tool stays visible.
func TestProfileToolsAllowAnMCPToolByIdOrByServer(t *testing.T) {
	for name, entry := range map[string]string{"exact": "mcp:fixture:lookup", "whole server": "mcp:fixture:*"} {
		t.Run(name, func(t *testing.T) {
			calls := &atomic.Int32{}
			f := buildSubagentFixtureWithMCP(t, map[string]any{
				"model": "child.json", "description": "Reads files.",
				"__session": profileSession(map[string]any{"allow": []string{entry}}),
			}, false, nil, &mcpFixture{calls: calls})
			status, raw := f.sessionTools(t)
			if status != http.StatusOK {
				t.Fatalf("tools route status %d: %s", status, raw)
			}
			var body struct {
				Tools  []struct{ ID, Source, Name string } `json:"tools"`
				Hidden []struct{ ID, Source, Name string } `json:"hidden"`
			}
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatal(err)
			}
			if got := mcpNames(body.Tools); got != "mcp:mcp:fixture:lookup" {
				t.Errorf("shown = %q, want only the MCP tool", got)
			}
			if got := mcpNames(body.Hidden); got != "native:native.read,native:native.subagent" {
				t.Errorf("hidden = %q, want read and subagent", got)
			}
		})
	}
}

// "mcp:<server>:*" names a server the session lists, even when the server shows no tool
// that matches; a server the session does not list is refused (covered above).
func TestProfileToolMatching(t *testing.T) {
	native := tools.Definition{ID: "native.read", Source: "native", Name: "read"}
	lookup := tools.Definition{ID: "mcp:docs:lookup", Source: "mcp", Server: "docs", Name: "mcp_docs_lookup_abc"}
	other := tools.Definition{ID: "mcp:docsx:lookup", Source: "mcp", Server: "docsx", Name: "mcp_docsx_lookup_abc"}
	for _, tc := range []struct {
		entry string
		def   tools.Definition
		want  bool
	}{
		{"read", native, true},
		{"write", native, false},
		{"read", lookup, false},
		{"read", tools.Definition{ID: "mcp:docs:read", Source: "mcp", Server: "docs", Name: "read"}, false},
		{"mcp:docs:lookup", lookup, true},
		{"mcp:docs:other", lookup, false},
		{"mcp:docs:*", lookup, true},
		{"mcp:docs:*", other, false},
		{"mcp:docs:*", native, false},
		{"mcp:docs:lookup", native, false},
	} {
		if got := profileToolMatches(tc.entry, tc.def); got != tc.want {
			t.Errorf("profileToolMatches(%q, %s) = %v, want %v", tc.entry, tc.def.ID, got, tc.want)
		}
	}
}

// The parent's profile shows only the subagent tool; the child still calls an MCP tool the
// parent cannot see, and the call reaches the server and returns.
func TestProfileToolsHideAnMCPToolFromTheParentButNotFromAChild(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{
		"model": "child.json", "tools": []string{"read", "mcp:fixture:lookup"},
		"__session": profileSession(map[string]any{"allow": []string{"subagent"}}),
	}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"answer"}`}

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) != 2 || len(child) != 2 {
		t.Fatalf("provider requests: parent %d (want 2), child %d (want 2)", len(parent), len(child))
	}
	if got := strings.Join(parent[0].Tools, ","); got != "subagent" {
		t.Errorf("parent tools = %q, want only subagent", got)
	}
	if got := strings.Join(child[0].Tools, ","); got != "read,"+mcpToolNameFor("fixture", "lookup") {
		t.Errorf("child tools = %q", got)
	}
	if calls.Load() != 1 || !strings.Contains(child[1].Text, "MCP says answer is 99") {
		t.Errorf("the MCP server saw %d calls (want 1); the child's second round:\n%s", calls.Load(), child[1].Text)
	}
}
