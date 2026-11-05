package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// hooksFixture is a writer child (exec) under a parent session whose setting.json carries
// the given hooks node (nil leaves the key out). The child writes made.txt with exec; an
// enabled hooks node scans exec's changes (shell_changes scan) so post_edit sees the file.
func hooksFixture(t *testing.T, hooks any) subagentFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	session := map[string]any{}
	if node, ok := hooks.(map[string]any); ok {
		if _, set := node["shell_changes"]; !set {
			node["shell_changes"] = "scan"
		}
	}
	if hooks != nil {
		session["hooks"] = hooks
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "exec"},
		"__exec":    map[string]any{},
		"__session": session,
	})
	f.provider.childFirstCall = [2]string{"exec", execWrite("made.txt", "by the child")}
	return f
}

func checkEntry(command string) map[string]any {
	return map[string]any{"name": "lint", "ext": []string{".txt"}, "argv": []string{"sh", "-c", command, "{rel}"}}
}

// childWriteResult is the exec call's output as the child's second request carried it.
func childWriteResult(t *testing.T, f subagentFixture) string {
	t.Helper()
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child requests = %d, want at least 2", len(child))
	}
	var found string
	for _, item := range child[1].Input {
		if strings.Contains(string(item), `"function_call_output"`) {
			found = string(item)
		}
	}
	if found == "" {
		t.Fatalf("the child's second request has no tool output: %v", child[1].Input)
	}
	return found
}

func TestHooksRunForAWriterChildAndTheModelSeesTheReport(t *testing.T) {
	f := hooksFixture(t, map[string]any{"enabled": true, "post_edit": []any{checkEntry(`echo "type error in $0"; exit 2`)}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v; a failing check must not undo the write", data, err)
	}
	output := childWriteResult(t, f)
	for _, want := range []string{"post-edit checks for made.txt", "[lint] failed, exit 2", "type error in made.txt"} {
		if !strings.Contains(output, want) {
			t.Errorf("the child's tool output lacks %q:\n%s", want, output)
		}
	}
}

func TestHooksOffLeaveTheToolOutputExactlyAsItWas(t *testing.T) {
	baseline := hooksFixture(t, nil)
	baseline.run(t)
	off := hooksFixture(t, map[string]any{"enabled": false, "post_edit": []any{checkEntry(`echo never`)}})
	off.run(t)
	// exec's summary carries the run time, which differs between two runs.
	elapsed := regexp.MustCompile(`completed in [0-9]+ ms`)
	same := func(text string) string { return elapsed.ReplaceAllString(text, "completed in <n> ms") }
	if a, b := same(childWriteResult(t, baseline)), same(childWriteResult(t, off)); a != b {
		t.Fatalf("a disabled hook changed the tool output:\n%s\n---\n%s", a, b)
	}
	if strings.Contains(childWriteResult(t, off), "post-edit") {
		t.Fatal("a disabled hook reported")
	}
}

// A child's tool call fires the session's tool-event hooks, and the payload names the agent.
func TestToolEventHooksRunForAChildAndNameTheAgent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "event.json")
	f := hooksFixture(t, map[string]any{"enabled": true, "pre_tool": []any{
		map[string]any{"name": "log", "match": "exec", "argv": []string{"sh", "-c", "cat > " + marker}},
	}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the hook did not run for the child's write: %v", err)
	}
	for _, want := range []string{`"event":"pre_tool"`, `"tool":"exec"`, `"agent":"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload lacks %q: %s", want, data)
		}
	}
	if strings.Contains(string(data), `"agent":""`) {
		t.Errorf("the child's agent name is empty: %s", data)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Errorf("the write itself changed: %q, %v", data, err)
	}
}

// A blocking pre_tool command stops a child's write before it touches the disk, and the
// model's next request carries the blocked result and the hook's report.
func TestBlockingPreToolHookStopsAChildWriteAndTheModelSeesWhy(t *testing.T) {
	f := hooksFixture(t, map[string]any{"enabled": true, "pre_tool": []any{
		map[string]any{"name": "freeze", "match": "exec", "blocking": true, "argv": []string{"sh", "-c", "echo writes-are-frozen; exit 1"}},
	}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); err == nil {
		t.Fatal("the write ran although a blocking pre_tool hook failed")
	}
	output := childWriteResult(t, f)
	for _, want := range []string{`blocked`, `blocked_by_hook`, `hook: freeze`, `writes-are-frozen`} {
		if !strings.Contains(output, want) {
			t.Errorf("the model's tool result lacks %q:\n%s", want, output)
		}
	}
}

// The same hook with blocking left off only reports: the write goes ahead.
func TestNonBlockingPreToolHookLetsAChildWriteGoAhead(t *testing.T) {
	f := hooksFixture(t, map[string]any{"enabled": true, "pre_tool": []any{
		map[string]any{"name": "freeze", "match": "exec", "argv": []string{"sh", "-c", "echo writes-are-frozen; exit 1"}},
	}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v; a non-blocking hook must not stop the write", data, err)
	}
}

// A child's call to a session MCP tool fires the session's tool-event hooks once each. The matcher
// names only the MCP tool, so the parent's own subagent call cannot fire them.
func TestToolEventHooksRunForAChildsSessionMCPCall(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "events")
	calls := &atomic.Int32{}
	hooks := map[string]any{"enabled": true,
		"pre_tool":  []any{map[string]any{"name": "pre", "match": "mcp:fixture:lookup", "argv": []string{"sh", "-c", "echo pre >> " + marker}}},
		"post_tool": []any{map[string]any{"name": "post", "match": "mcp:fixture:lookup", "argv": []string{"sh", "-c", "echo post >> " + marker}}},
	}
	f := buildChildMCPHookFixture(t, hooks, calls)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if calls.Load() != 1 {
		t.Fatalf("the MCP server ran %d times, want 1", calls.Load())
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no hook ran for the child's MCP call: %v", err)
	}
	if got := string(data); got != "pre\npost\n" {
		t.Fatalf("hook events = %q, want one pre_tool then one post_tool", got)
	}
}

// A blocking pre_tool command stops a child's session MCP call: the server never runs, and the
// model's next request carries the blocked result.
func TestBlockingPreToolHookStopsAChildsSessionMCPCall(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildChildMCPHookFixture(t, map[string]any{"enabled": true, "pre_tool": []any{
		map[string]any{"name": "freeze", "match": "mcp:fixture:lookup", "blocking": true, "argv": []string{"sh", "-c", "echo mcp-is-frozen; exit 1"}},
	}}, calls)
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if calls.Load() != 0 {
		t.Fatalf("the MCP server ran %d times although a blocking pre_tool hook failed", calls.Load())
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child made %d requests", len(child))
	}
	history := child[len(child)-1].Text
	for _, want := range []string{"blocked_by_hook", "hook: freeze", "mcp-is-frozen"} {
		if !strings.Contains(history, want) {
			t.Errorf("the child's history lacks %q:\n%s", want, history)
		}
	}
}

// buildChildMCPHookFixture is a child that lists one session MCP tool, which the child calls first.
func buildChildMCPHookFixture(t *testing.T, hooks map[string]any, calls *atomic.Int32) subagentFixture {
	t.Helper()
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "mcp:fixture:lookup"}}
	session := profileSession(profileTools)
	session["hooks"] = hooks
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "mcp:fixture:lookup"}, "__session": session}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"answer"}`}
	return f
}

// A child's refused call to an unloaded lazy session MCP tool fires the owner's pre_tool and
// post_tool once each, and the server never runs.
func TestRefusedLazyCallFiresAChildHooksOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "events")
	calls := &atomic.Int32{}
	name := mcpToolNameFor("fixture", "lookup")
	profileTools := map[string]any{"allow": []string{"subagent", "skill", "load_tool", "mcp:fixture:lookup"}, "lazy": []string{"mcp:fixture:lookup"}}
	session := profileSession(profileTools)
	session["hooks"] = map[string]any{"enabled": true,
		"pre_tool":  []any{map[string]any{"name": "pre", "match": "mcp:fixture:lookup", "argv": []string{"sh", "-c", "echo pre >> " + marker}}},
		"post_tool": []any{map[string]any{"name": "post", "match": "mcp:fixture:lookup", "argv": []string{"sh", "-c", "echo post >> " + marker}}},
	}
	f := buildSubagentFixtureWithMCP(t, map[string]any{"model": "child.json", "tools": []string{"skill", "mcp:fixture:lookup"}, "__session": session, "__load_tool": true}, false, nil, &mcpFixture{calls: calls})
	f.provider.childFirstCall = [2]string{name, `{}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if calls.Load() != 0 {
		t.Fatalf("the MCP server ran %d times for an unloaded lazy tool", calls.Load())
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no hook ran for the refused call: %v", err)
	}
	if got := string(data); got != "pre\npost\n" {
		t.Fatalf("hook events = %q, want one pre_tool then one post_tool", got)
	}
}
