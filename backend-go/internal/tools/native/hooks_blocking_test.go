package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func blockingHook(name, match, script string) sessionfs.ToolEventHook {
	hook := eventHook(name, match, script)
	hook.Blocking = true
	return hook
}

func TestBlockingPreToolHookStopsTheCallAndReportsWhy(t *testing.T) {
	root := t.TempDir()
	postMark := filepath.Join(root, "post-ran")
	laterMark := filepath.Join(root, "later-ran")
	settings := eventSettings(
		[]sessionfs.ToolEventHook{
			blockingHook("gate", "*", `echo no-writes-today; exit 3`),
			eventHook("later", "*", `touch `+laterMark),
		},
		[]sessionfs.ToolEventHook{eventHook("after", "*", `touch `+postMark)},
	)
	tool, calls := recordingTool("write", "write", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{})
	if *calls != 0 {
		t.Fatalf("the tool ran %d times although a blocking hook failed", *calls)
	}
	for _, want := range []string{`"id":"c1"`, `"status":"blocked"`, "error_code: blocked_by_hook", "tool: write", "hook: gate", "pre-tool hooks for write", "[gate] failed, exit 3", "no-writes-today"} {
		if !strings.Contains(output, want) {
			t.Errorf("blocked result lacks %q:\n%s", want, output)
		}
	}
	for _, mark := range []string{postMark, laterMark} {
		if _, err := os.Stat(mark); err == nil {
			t.Errorf("%s exists: a hook ran after the block", filepath.Base(mark))
		}
	}
}

func TestBlockingPreToolHookLetsAPassingCallThrough(t *testing.T) {
	root := t.TempDir()
	settings := eventSettings([]sessionfs.ToolEventHook{blockingHook("gate", "*", `exit 0`)}, nil)
	tool, calls := recordingTool("read", "read", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), root, map[string]any{})
	if *calls != 1 || !strings.Contains(output, `"status":"ok"`) || strings.Contains(output, "blocked") {
		t.Fatalf("calls=%d output=%s; a passing blocking hook must not stop the call", *calls, output)
	}
}

func TestBlockingPreToolHookTreatsEveryNonPassAsABlock(t *testing.T) {
	cases := []struct {
		name string
		hook sessionfs.ToolEventHook
		want string
	}{
		{"signal", blockingHook("g", "*", `kill -TERM $$`), "signal"},
		{"missing program", sessionfs.ToolEventHook{Name: "g", Match: "*", Blocking: true, Argv: []string{"/nonexistent/program"}}, "could not start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			settings := eventSettings([]sessionfs.ToolEventHook{c.hook}, nil)
			tool, calls := recordingTool("read", "read", "native")
			output := runEventTool(t, WithToolEventHooks(tool, settings, ""), t.TempDir(), map[string]any{})
			if *calls != 0 || !strings.Contains(output, `"status":"blocked"`) || !strings.Contains(output, c.want) {
				t.Fatalf("calls=%d want %q in:\n%s", *calls, c.want, output)
			}
		})
	}
}

func TestBlockingPreToolHookBlocksATimedOutCommand(t *testing.T) {
	settings := eventSettings([]sessionfs.ToolEventHook{blockingHook("slow", "*", `sleep 5`)}, nil)
	settings.TimeoutMS = 300
	tool, calls := recordingTool("read", "read", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), t.TempDir(), map[string]any{})
	if *calls != 0 || !strings.Contains(output, `"status":"blocked"`) || !strings.Contains(output, "timed out") {
		t.Fatalf("calls=%d output=%s", *calls, output)
	}
}

func TestNonBlockingPreToolHookStillOnlyReports(t *testing.T) {
	settings := eventSettings([]sessionfs.ToolEventHook{eventHook("note", "*", `exit 3`)}, nil)
	tool, calls := recordingTool("read", "read", "native")
	output := runEventTool(t, WithToolEventHooks(tool, settings, ""), t.TempDir(), map[string]any{})
	if *calls != 1 || !strings.Contains(output, `"status":"ok"`) || !strings.Contains(output, "[note] failed, exit 3") {
		t.Fatalf("calls=%d output=%s", *calls, output)
	}
}

func TestBlockingHookOnlyBlocksTheToolsItMatches(t *testing.T) {
	settings := eventSettings([]sessionfs.ToolEventHook{blockingHook("gate", "write", `exit 1`)}, nil)
	read, readCalls := recordingTool("read", "read", "native")
	write, writeCalls := recordingTool("write", "write", "native")
	runEventTool(t, WithToolEventHooks(read, settings, ""), t.TempDir(), map[string]any{})
	runEventTool(t, WithToolEventHooks(write, settings, ""), t.TempDir(), map[string]any{})
	if *readCalls != 1 || *writeCalls != 0 {
		t.Fatalf("read ran %d times, write ran %d times; want 1 and 0", *readCalls, *writeCalls)
	}
}

func TestBlockingPreToolHookBlocksAnExecuteOutputTool(t *testing.T) {
	ran := 0
	tool := tools.Tool{
		Definition: tools.Definition{ID: "mcp:docs:search", Source: "mcp", Name: "docs_search"},
		ExecuteOutput: func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
			ran++
			return json.RawMessage(`[{"type":"input_text","text":"hit"}]`), nil
		},
	}
	settings := eventSettings([]sessionfs.ToolEventHook{blockingHook("gate", "mcp:docs:*", `echo stop; exit 1`)}, nil)
	wrapped := WithToolEventHooks(tool, settings, "")
	output, err := wrapped.ExecuteOutput(context.Background(), map[string]any{}, tools.ExecutionContext{CallID: "c9", WorkspaceRoot: t.TempDir()})
	if err != nil || ran != 0 {
		t.Fatalf("err=%v ran=%d", err, ran)
	}
	var text string
	if json.Unmarshal(output, &text) != nil || !strings.Contains(text, `"status":"blocked"`) || !strings.Contains(text, "hook: gate") || !strings.Contains(text, "stop") {
		t.Fatalf("output = %s", output)
	}
}
