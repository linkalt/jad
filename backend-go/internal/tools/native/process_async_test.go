package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

type processAsyncFixture struct {
	manager *ManagedProcessManager
	host    *AsyncHost
	process tools.Tool
	async   tools.Tool
	dir     string
}

func newProcessAsyncFixture(t *testing.T) processAsyncFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	dir := t.TempDir()
	manager := NewProcessManager(filepath.Join(dir, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	settings := processTestRequest(dir, "", 1<<20).Policy
	settings.StorageDir = filepath.Join(dir, "processes")
	settings.Shell = "/bin/sh"
	process, err := NewProcessToolWithAsync(settings, manager, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return processAsyncFixture{manager: manager, host: host, process: process, async: asyncTool, dir: dir}
}

func (f processAsyncFixture) start(t *testing.T, owner, command string) string {
	t.Helper()
	out, err := f.process.Execute(context.Background(), map[string]any{"command": command}, tools.ExecutionContext{CallID: "p1", WorkspaceRoot: f.dir, SessionKey: owner})
	if err != nil {
		t.Fatal(err)
	}
	return processIDOf(t, out)
}

func (f processAsyncFixture) wait(t *testing.T, owner string, waitMs int64) string {
	t.Helper()
	return callAsync(t, f.async, owner, map[string]any{"action": "wait", "wait_ms": json.Number(time.Duration(waitMs).String())[:0] + jsonInt(waitMs)})
}

func jsonInt(value int64) json.Number { return json.Number(strings.TrimSpace(string(mustJSON(value)))) }

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func TestProcessStartIsAnAsyncHandleOwnedByTheSession(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "sleep 30")
	entry, err := f.host.Manager().Status("s1", id)
	if err != nil || entry.Kind != "process" || entry.State != asyncmgr.StateRunning {
		t.Fatalf("entry = %+v, err = %v", entry, err)
	}
	if _, err := f.host.Manager().Status("s2", id); err == nil {
		t.Fatal("another session sees the handle")
	}
	if list := callAsync(t, f.async, "s1", map[string]any{}); !strings.Contains(list, id) || !strings.Contains(list, "sleep 30") {
		t.Fatalf("list = %s", list)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestProcessExitQueuesASignalWithTheExitCodeAndARef(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "echo out-text; echo err-text >&2; exit 3")
	out := f.wait(t, "s1", 5000)
	for _, want := range []string{id, "done", "err-text", "pending: 0", filepath.Join(f.dir, "processes", id)} {
		if !strings.Contains(out, want) {
			t.Errorf("wait output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\"code\":\"3\"") || strings.Contains(out, "\"ok\":true") {
		t.Errorf("want code 3 and ok:false:\n%s", out)
	}
	ok := f.start(t, "s1", "echo fine")
	out = f.wait(t, "s1", 5000)
	if !strings.Contains(out, ok) || !strings.Contains(out, "\"ok\":true") || !strings.Contains(out, "fine") {
		t.Errorf("exit 0 signal:\n%s", out)
	}
}

func TestProcessAsyncStopIsNonBlockingAndEndsTheGroup(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "sleep 30 & sleep 30; wait")
	snapshot, _ := f.manager.Snapshot(id)
	pid := snapshot.PID
	begun := time.Now()
	f.host.Manager().CancelOwner("s1", "user_stop")
	if took := time.Since(begun); took > 500*time.Millisecond {
		t.Fatalf("CancelOwner blocked for %v", took)
	}
	out := f.wait(t, "s1", 10000)
	if !strings.Contains(out, id) || !strings.Contains(out, "stopped") || !strings.Contains(out, "user_stop") {
		t.Fatalf("wait after cancel:\n%s", out)
	}
	if processGroupAlive(pid) {
		t.Fatalf("process group %d is still alive after the stop (P-037 group kill)", pid)
	}
}

func TestProcessAsyncStopThroughTheTool(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "sleep 30")
	out := callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
	if !strings.Contains(out, "stopped") || !strings.Contains(out, "stopped_by_agent") {
		t.Fatalf("stop = %s", out)
	}
	if again := callAsync(t, f.async, "s1", map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(again, id) {
		t.Fatalf("stop must consume the signal it returned:\n%s", again)
	}
}

func TestProcessStatusShowsLiveOutputThroughTheDriver(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "echo live-line; sleep 30")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := callAsync(t, f.async, "s1", map[string]any{"action": "status", "id": id})
		if strings.Contains(out, "live-line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never showed the output:\n%s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	callAsync(t, f.async, "s1", map[string]any{"action": "stop", "id": id})
}

func TestProcessCapacityRefusalIsReportedAndLeavesNoHandle(t *testing.T) {
	f := newProcessAsyncFixture(t)
	cfg := asyncmgr.DefaultConfig()
	cfg.MaxHandles = 1
	host, err := NewAsyncHost(filepath.Join(f.dir, ".async2"), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	settings := processTestRequest(f.dir, "", 1<<20).Policy
	settings.StorageDir = filepath.Join(f.dir, "processes2")
	manager := NewProcessManager(settings.StorageDir)
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := NewProcessToolWithAsync(settings, manager, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	run := func() string {
		out, _ := tool.Execute(context.Background(), map[string]any{"command": "sleep 30"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: f.dir, SessionKey: "s1"})
		return out
	}
	first := run()
	second := run()
	if !strings.Contains(second, "async_capacity_reached") {
		t.Fatalf("second start = %s", second)
	}
	if running := manager.List("running"); len(running) != 1 || running[0].ID != processIDOf(t, first) {
		t.Fatalf("a refused start left a process: %v", running)
	}
}

func TestChildProcessesAreNotAsyncHandles(t *testing.T) {
	f := newProcessAsyncFixture(t)
	out, err := f.process.Execute(context.Background(), map[string]any{"command": "sleep 30"}, tools.ExecutionContext{CallID: "c", WorkspaceRoot: f.dir, SessionKey: "s1", ProcessOwner: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	id := processIDOf(t, out)
	if _, err := f.host.Manager().Status("s1", id); err == nil {
		t.Fatal("a child's process became a handle of the parent session")
	}
	f.manager.stopOwned("run_1", "test")
}

func TestOldProcessWaitConsumesTheSignalItReturned(t *testing.T) {
	f := newProcessAsyncFixture(t)
	id := f.start(t, "s1", "echo quick")
	out, err := f.process.Execute(context.Background(), map[string]any{"action": "wait", "id": id, "timeout_ms": json.Number("5000")}, tools.ExecutionContext{CallID: "w", WorkspaceRoot: f.dir, SessionKey: "s1"})
	if err != nil || !strings.Contains(out, "quick") {
		t.Fatalf("wait = %s, err = %v", out, err)
	}
	if later := f.wait(t, "s1", 0); strings.Contains(later, id) {
		t.Fatalf("the signal was delivered twice:\n%s", later)
	}
}
