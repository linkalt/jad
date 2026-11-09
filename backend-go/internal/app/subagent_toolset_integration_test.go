package app

import (
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// The session registers only the subagent tool. The agent's toolset gives the child
// skill, which the child uses to load the notes; the parent is never sent it.
func TestSubagentToolsetGivesTheChildAToolTheSessionDoesNotRegister(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.", "toolset": "extra/reader",
		"__session_without": []string{"skill"},
		"__toolsets":        map[string]any{"extra/reader": map[string]any{"native": map[string]any{"skill": map[string]any{}}}},
	})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) == 0 || len(child) < 2 {
		t.Fatalf("requests: parent %d, child %d", len(parent), len(child))
	}
	if got := strings.Join(parent[0].Tools, ","); got != "async,subagent" {
		t.Errorf("parent tools = %q, want only async and subagent", got)
	}
	if got := strings.Join(child[0].Tools, ","); got != "skill,subagent" {
		t.Errorf("child tools = %q, want skill from its toolset and the session's subagent", got)
	}
	if !strings.Contains(child[1].Text, "the secret is 42") {
		t.Errorf("the child's skill call did not run:\n%s", child[1].Text)
	}
}

// An agent without a toolset still takes the session's tools when another agent names one.
func TestSubagentWithoutToolsetKeepsTheSessionTools(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "description": "Reads files.",
		"__toolsets": map[string]any{"extra/reader": map[string]any{"native": map[string]any{"skill": map[string]any{}}}},
	})
	f.run(t)
	child := f.provider.forModel("child-model")
	if len(child) == 0 || strings.Join(child[0].Tools, ",") != "async,skill,subagent" {
		t.Fatalf("child requests %+v, want the session's async, skill and subagent", child)
	}
}

// A toolset that is not in the catalog fails the run's configuration with the available
// ones listed, and the provider is never called.
func TestSubagentToolsetThatDoesNotExistRefusesTheRun(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "toolset": "extra/missing"})
	status, body := f.runStatus(t, "ask the explorer")
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `toolset "extra/missing" is not available here (available: coding/default`) {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if len(f.provider.forModel("parent-model")) != 0 {
		t.Error("the provider was called for a session that cannot load")
	}
}

// The session lists no MCP server. The agent's toolset does, and the child calls the
// server's tool through it; the parent is never sent that tool.
func TestSubagentToolsetConnectsItsOwnMCPServer(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{
		"model": "child.json", "toolset": "extra/lookup", "tools": []string{"mcp:fixture:lookup"},
		"__toolsets": map[string]any{"extra/lookup": map[string]any{
			"native": map[string]any{},
			"mcp":    map[string]any{"m/fixture": map[string]any{"deny_tools": []string{}}},
		}},
	}, false, nil, &mcpFixture{calls: calls, onlyFile: true})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"answer"}`}

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if calls.Load() != 1 {
		t.Errorf("the MCP tool was called %d times, want 1", calls.Load())
	}
	parent := f.provider.forModel("parent-model")
	child := f.provider.forModel("child-model")
	if len(parent) == 0 || len(child) < 2 {
		t.Fatalf("requests: parent %d, child %d", len(parent), len(child))
	}
	for _, name := range parent[0].Tools {
		if strings.Contains(name, "lookup") {
			t.Errorf("the parent was sent %q, a tool of the agent's toolset", name)
		}
	}
	if got := strings.Join(child[0].Tools, ","); got != mcpToolNameFor("fixture", "lookup") {
		t.Errorf("child tools = %q, want only the MCP tool", got)
	}
	if !strings.Contains(child[1].Text, "MCP says answer is 99") {
		t.Errorf("the child did not get the MCP result:\n%s", child[1].Text)
	}
}

// An mcp: entry that names a server the toolset does not list is refused at load, with
// the toolset named; the session's own server list does not count.
func TestSubagentToolsetRefusesAnMCPToolOfAnotherServer(t *testing.T) {
	f := buildSubagentFixtureWithMCP(t, map[string]any{
		"model": "child.json", "toolset": "extra/reader", "tools": []string{"skill", "mcp:fixture:lookup"},
		"__toolsets": map[string]any{"extra/reader": map[string]any{"native": map[string]any{"skill": map[string]any{}}}},
	}, false, nil, &mcpFixture{calls: &atomic.Int32{}})
	status, body := f.sessionTools(t)
	body = errorText([]byte(body))
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"mcp:fixture:lookup", which is not a loaded MCP tool of toolset extra/reader (loaded: none)`) {
		t.Fatalf("tools route: status %d, body %s", status, body)
	}
}

// An exec that comes from a toolset hides the running model's API key variable from the
// child's shell even when the toolset's own env sets it, as the session's own exec does.
// The toolset is loaded before the model file is read, so it must be given the model.
func TestSubagentToolsetExecHidesTheModelsAPIKeyFromTheChild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "toolset": "extra/shell", "tools": []string{"exec"},
		"__exec":            map[string]any{},
		"__session_without": []string{"exec"},
		"__toolsets": map[string]any{"extra/shell": map[string]any{"native": map[string]any{
			"exec": map[string]any{"spill": map[string]any{"enabled": false}, "env": map[string]any{"FIXTURE_OPENAI_KEY": "secret-provider-value"}},
		}}},
	})
	f.provider.childFirstCall = [2]string{"exec", `{"code":"echo KEY=[$FIXTURE_OPENAI_KEY]"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child requests = %d, want at least 2", len(child))
	}
	if strings.Contains(child[1].Text, "secret-provider-value") {
		t.Errorf("the model's API key reached the child's shell:\n%s", child[1].Text)
	}
	if !strings.Contains(child[1].Text, "KEY=[]") {
		t.Errorf("the child's exec did not run:\n%s", child[1].Text)
	}
}

// The profile's tools.deny reaches the toolset's tools: skill and subagent are denied, so the toolset
// gives the agent nothing and the session refuses to build, naming the deny.
func TestSubagentToolsetToolDeniedByTheProfileIsNotGiven(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "toolset": "extra/reader",
		"__session":  profileSession(map[string]any{"deny": []string{"skill", "subagent"}}),
		"__toolsets": map[string]any{"extra/reader": map[string]any{"native": map[string]any{"skill": map[string]any{}}}},
	})
	status, body := f.runStatus(t, "ask the explorer")
	if status != http.StatusOK || !strings.Contains(body, "event: error") {
		t.Fatalf("run: status %d, want 200 with an error event, body %s", status, body)
	}
	status, body = f.sessionTools(t)
	body = errorText([]byte(body))
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "agents.explore has no tool to use") || !strings.Contains(body, "the profile's tools.deny is in effect: skill, subagent") {
		t.Fatalf("tools route: status %d, body %s", status, body)
	}
	if len(f.provider.forModel("parent-model")) != 0 {
		t.Error("the provider was called for a session that cannot build its tools")
	}
}
