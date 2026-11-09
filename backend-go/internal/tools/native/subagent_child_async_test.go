package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestChildGetsAsyncOnlyWhenItsToolsListNamesIt(t *testing.T) {
	host := newTestAsyncHost(t)
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	parent := append(withExecutors(t, readOnlyTool("read")), async)
	listsAsync := agentNamed("waiter")
	listsAsync.Tools, listsAsync.HasTools = []string{"read", "async"}, true

	for name, tc := range map[string]struct {
		agent sessionfs.NativeSubagentAgent
		want  string
	}{
		"default tools": {agentNamed("plain"), "read,async"},
		"listed":        {listsAsync, "read,async"},
	} {
		t.Run(name, func(t *testing.T) {
			launcher := &fakeLauncher{}
			tool, err := newSubagentToolAsync(subagentSettings(tc.agent), parent, nil, launcher, nil, NewSubagentManager(), host)
			if err != nil {
				t.Fatal(err)
			}
			catalog := catalogWithAsync(t, host, withExecutors(t, readOnlyTool("read")), tool)
			subagentCall(t, catalog, "{\"task\":\"x\",\"block_ms\":2000}")
			if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != tc.want {
				t.Fatalf("child tools = %q, want %q", got, tc.want)
			}
		})
	}
}

var exitSevenPattern = regexp.MustCompile(`"code":"7"`)

// A child with exec and async starts a program with a short block, waits for it with its
// own async tool and reads the exit code; its async tool never sees or stops a handle of
// the parent; its live handles are stopped when it ends and the parent never sees them.
func TestChildAsyncIsScopedToTheChildsOwnRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec process groups require a POSIX platform")
	}
	requireInterpreters(t, "bash")
	baseline := baselineCount()
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, dir)
	settings.Child.TimeoutMs, settings.Child.MaxTimeoutMs = 20_000, 30_000
	process, err := NewExecTool(settings, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSubagentManager()

	const parentKey = "sess|a"
	parentExec := tools.ExecutionContext{CallID: "p", WorkspaceRoot: dir, SessionDir: dir, SessionKey: parentKey}
	out, err := process.Execute(context.Background(), map[string]any{"lang": "bash", "code": "sleep 30", "block_ms": json.Number("0")}, parentExec)
	if err != nil {
		t.Fatal(err)
	}
	parentProcess := execRunIDOf(t, out)

	var ids struct{ finished, stopped, left, run string }
	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, strings.TrimSpace(strings.Join(strings.Fields(format), " "))+" "+toJSON(args))
	}
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		exec := request.Execution
		exec.CallID = "c"
		exec.WorkspaceRoot = dir
		exec.SessionDir = dir
		ids.run = exec.ProcessOwner
		call := func(tool tools.Tool, arguments map[string]any) string {
			if tool.NormalizeArguments != nil {
				arguments = tool.NormalizeArguments(arguments)
			}
			result, err := tool.Execute(ctx, arguments, exec)
			if err != nil {
				fail("call error", err.Error())
			}
			return result
		}
		ids.finished = execRunIDOf(t, call(process, map[string]any{"lang": "bash", "code": "sleep 0.3; exit 7", "block_ms": json.Number("50")}))
		if got := call(async, map[string]any{"action": "wait", "wait_ms": json.Number("5000")}); !exitSevenPattern.MatchString(got) || !strings.Contains(got, ids.finished) {
			fail("child wait did not give the exit code", got)
		}
		ids.stopped = execRunIDOf(t, call(process, map[string]any{"lang": "bash", "code": "sleep 30", "block_ms": json.Number("0")}))
		ids.left = execRunIDOf(t, call(process, map[string]any{"lang": "bash", "code": "sleep 30", "block_ms": json.Number("0")}))
		listed := call(async, map[string]any{"action": "list"})
		if !strings.Contains(listed, ids.stopped) || !strings.Contains(listed, ids.left) || strings.Contains(listed, parentProcess) {
			fail("child list must hold its own handles only", listed)
		}
		for _, action := range []string{"status", "stop"} {
			got := call(async, map[string]any{"action": action, "id": parentProcess})
			if !strings.Contains(got, "unknown_handle") {
				fail("child "+action+" of the parent's handle must be not found", got)
			}
		}
		if got := call(async, map[string]any{"action": "stop", "id": ids.stopped}); !strings.Contains(got, ids.stopped) {
			fail("child stop of its own handle", got)
		}
		return SubagentResult{Text: "done", Rounds: 1, Tokens: 1}, nil
	}}

	agent := writerAgent("runner", "read", "exec", "async")
	parentTools := append(withExecutors(t, readOnlyTool("read")), process, async)
	sub, err := newSubagentToolAsync(writeSettings(agent), parentTools, nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	out, err = sub.Execute(context.Background(), map[string]any{"task": "serve"}, tools.ExecutionContext{CallID: "s", WorkspaceRoot: dir, SessionKey: parentKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatalf("inside the child:\n%s\nresult:\n%s", strings.Join(failures, "\n"), out)
	}

	// A run still in the host's list has a live child; the child's two long runs must be
	// gone (stopped) and the parent's must still run.
	running := func(id string) bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.shellRuns[id] != nil
	}
	settle := time.Now().Add(5 * time.Second)
	for (running(ids.left) || running(ids.stopped)) && time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
	}
	if running(ids.left) {
		t.Errorf("the child's live handle %s still runs after the child ended", ids.left)
	}
	if running(ids.stopped) {
		t.Errorf("the child's stopped handle %s still runs", ids.stopped)
	}
	if entry, err := host.Manager().Status(parentKey, parentProcess); err != nil || entry.State != asyncmgr.StateRunning {
		t.Errorf("the parent's handle is %+v, %v: a child must not touch it", entry, err)
	}
	for _, action := range []map[string]any{{"action": "list", "state": "all"}, {"action": "wait", "wait_ms": json.Number("0")}} {
		got := callAsync(t, async, parentKey, action)
		for _, id := range []string{ids.finished, ids.stopped, ids.left} {
			if strings.Contains(got, id) {
				t.Errorf("the parent's async %v shows the child's handle %s:\n%s", action["action"], id, got)
			}
		}
	}
	if counts := host.Manager().Counts(parentKey); counts.Active != 1 || counts.Unread != 0 {
		t.Errorf("parent counts = %+v, want one active handle and nothing unread", counts)
	}
	if _, ids := host.Manager().Snapshot(parentKey); len(ids) != 1 || ids[0] != parentProcess {
		t.Errorf("the parent's SSE ids = %v, want only its own handle", ids)
	}
	if counts := host.Manager().Counts(ids.run); counts != (asyncmgr.Counts{}) {
		t.Errorf("the ended child still owns handles: %+v", counts)
	}
	if got := callAsync(t, async, ids.run, map[string]any{"action": "status", "id": ids.left}); !strings.Contains(got, "unknown_handle") {
		t.Errorf("a handle of the ended child is still readable:\n%s", got)
	}

	callAsync(t, async, parentKey, map[string]any{"action": "stop", "id": parentProcess})
	deadline := time.Now().Add(5 * time.Second)
	for running(parentProcess) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = manager.Close()
	host.Close()
	requireNoGrowth(t, baseline, "a child with async ended")
}

func toJSON(values []any) string {
	data, _ := json.Marshal(values)
	return string(data)
}
