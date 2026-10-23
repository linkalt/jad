package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hooksFixture is a writer child (write + bash) under a parent session whose setting.json
// carries the given hooks node (nil leaves the key out). The child writes made.txt.
func hooksFixture(t *testing.T, hooks any) subagentFixture {
	t.Helper()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	session := map[string]any{}
	if hooks != nil {
		session["hooks"] = hooks
	}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"read", "write"},
		"__write":   map[string]any{"max_bytes": 4096},
		"__bash":    map[string]any{"shell": shell, "spill": map[string]any{"enabled": false}},
		"__session": session,
	})
	f.provider.childFirstCall = [2]string{"write", `{"path":"made.txt","content":"by the child"}`}
	return f
}

func checkEntry(command string) map[string]any {
	return map[string]any{"name": "lint", "ext": []string{".txt"}, "argv": []string{"sh", "-c", command, "{rel}"}}
}

// childWriteResult is the write tool's output as the child's second request carried it.
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
	for _, want := range []string{"Created made.txt", "post-edit checks for made.txt", "[lint] failed, exit 2", "type error in made.txt"} {
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
	if a, b := childWriteResult(t, baseline), childWriteResult(t, off); a != b {
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
		map[string]any{"name": "log", "match": "write", "argv": []string{"sh", "-c", "cat > " + marker}},
	}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the hook did not run for the child's write: %v", err)
	}
	for _, want := range []string{`"event":"pre_tool"`, `"tool":"write"`, `"agent":"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload lacks %q: %s", want, data)
		}
	}
	if strings.Contains(string(data), `"agent":""`) {
		t.Errorf("the child's agent name is empty: %s", data)
	}
	if output := childWriteResult(t, f); !strings.Contains(output, "Created made.txt") {
		t.Errorf("the write itself changed: %s", output)
	}
}

// A blocking pre_tool command stops a child's write before it touches the disk, and the
// model's next request carries the blocked result and the hook's report.
func TestBlockingPreToolHookStopsAChildWriteAndTheModelSeesWhy(t *testing.T) {
	f := hooksFixture(t, map[string]any{"enabled": true, "pre_tool": []any{
		map[string]any{"name": "freeze", "match": "write", "blocking": true, "argv": []string{"sh", "-c", "echo writes-are-frozen; exit 1"}},
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
		map[string]any{"name": "freeze", "match": "write", "argv": []string{"sh", "-c", "echo writes-are-frozen; exit 1"}},
	}})
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if data, err := os.ReadFile(filepath.Join(f.workspace, "made.txt")); err != nil || string(data) != "by the child" {
		t.Fatalf("made.txt = %q, %v; a non-blocking hook must not stop the write", data, err)
	}
}
