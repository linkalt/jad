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

type mixedHandles struct {
	host    *AsyncHost
	proc    *ManagedProcessManager
	process tools.Tool
	sub     tools.Tool
	async   tools.Tool
	dir     string
	close   func()
}

func newMixedHandles(t *testing.T) mixedHandles {
	t.Helper()
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	processes := NewProcessManager(filepath.Join(dir, "processes"))
	settings := processTestRequest(dir, "", 1<<20).Policy
	settings.StorageDir = filepath.Join(dir, "processes")
	settings.Shell = "/bin/sh"
	process, err := NewProcessToolWithAsync(settings, processes, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: filepath.Join(dir, "sub"), Keep: 10})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		return SubagentResult{Text: "partial"}, ctx.Err()
	}}
	sub, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), withExecutors(t, readOnlyTool("read")), nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return mixedHandles{host: host, proc: processes, process: process, sub: sub, async: async, dir: dir, close: func() {
		_ = manager.Close()
		_ = processes.Close()
		host.Close()
	}}
}

func (m mixedHandles) startBoth(t *testing.T, owner string) (processID, runID string) {
	t.Helper()
	exec := tools.ExecutionContext{CallID: "c", WorkspaceRoot: m.dir, SessionKey: owner}
	out, err := m.process.Execute(context.Background(), map[string]any{"command": "sleep 30 & sleep 30; wait", "block_ms": json.Number("0")}, exec)
	if err != nil {
		t.Fatal(err)
	}
	processID = processIDOf(t, out)
	out, err = m.sub.Execute(context.Background(), map[string]any{"task": "research", "block_ms": json.Number("0")}, exec)
	if err != nil {
		t.Fatal(err)
	}
	return processID, runIDOf(t, out)
}

func waitForState(t *testing.T, host *AsyncHost, owner, id string, want asyncmgr.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, err := host.Manager().Status(owner, id); err == nil && entry.State == want {
			return
		}
		if time.Now().After(deadline) {
			entry, err := host.Manager().Status(owner, id)
			t.Fatalf("%s = %+v, %v; want %s", id, entry, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserStopResetAndDeleteReachProcessAndSubagentHandles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	before := baselineCount()
	{
		m := newMixedHandles(t)
		manager := m.host.Manager()

		// stop: both end as stopped with reason user_stop, and the signals are queued.
		p1, r1 := m.startBoth(t, "sess|a")
		if n := manager.CancelOwner("sess|a", "user_stop"); n != 2 {
			t.Fatalf("CancelOwner asked %d, want 2", n)
		}
		waitForState(t, m.host, "sess|a", p1, asyncmgr.StateStopped)
		waitForState(t, m.host, "sess|a", r1, asyncmgr.StateStopped)
		if got := callAsync(t, m.async, "sess|a", map[string]any{"action": "wait", "wait_ms": json.Number("0")}); !strings.Contains(got, p1) || !strings.Contains(got, r1) || !strings.Contains(got, "user_stop") {
			t.Errorf("wait after a user stop:\n%s", got)
		}

		// reset: both end as stopped, nothing is delivered, an earlier unread signal is gone.
		p2, r2 := m.startBoth(t, "sess|b")
		other, otherRun := m.startBoth(t, "sess|c")
		if n := manager.ResetOwner("sess|b", "session_reset"); n != 2 {
			t.Fatalf("ResetOwner asked %d, want 2", n)
		}
		waitForState(t, m.host, "sess|b", p2, asyncmgr.StateStopped)
		waitForState(t, m.host, "sess|b", r2, asyncmgr.StateStopped)
		if got := callAsync(t, m.async, "sess|b", map[string]any{"action": "wait", "wait_ms": json.Number("0")}); strings.Contains(got, p2) || strings.Contains(got, r2) {
			t.Errorf("wait after a reset delivered a signal:\n%s", got)
		}

		// delete: the session's handles are stopped and forgotten; sess|c is untouched.
		if n := manager.DeleteOwnersWithPrefix("sess|b", "session_delete"); n != 0 {
			t.Fatalf("delete of finished handles asked %d", n)
		}
		if _, err := manager.Status("sess|b", p2); err == nil {
			t.Error("a deleted session's finished handle is still known")
		}
		p3, r3 := m.startBoth(t, "sess|d")
		if n := manager.DeleteOwnersWithPrefix("sess|d", "session_delete"); n != 2 {
			t.Fatalf("delete asked %d, want 2", n)
		}
		if snapshot, _ := m.proc.Snapshot(p3); snapshot.State == ProcessRunning {
			deadline := time.Now().Add(5 * time.Second)
			for snapshot.State == ProcessRunning && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				snapshot, _ = m.proc.Snapshot(p3)
			}
			if snapshot.State == ProcessRunning {
				t.Error("the deleted session's process is still running")
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for manager.Counts("sess|d").Total != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if c := manager.Counts("sess|d"); c.Total != 0 || c.Unread != 0 {
			t.Errorf("deleted session still has handles: %+v (%s %s)", c, p3, r3)
		}
		for _, id := range []string{other, otherRun} {
			if entry, err := manager.Status("sess|c", id); err != nil || entry.State.Terminal() {
				t.Errorf("another session's handle %s = %+v, %v", id, entry, err)
			}
		}
		if snapshot, _ := m.proc.Snapshot(other); snapshot.State != ProcessRunning {
			t.Errorf("another session's process = %+v", snapshot)
		}
		m.close()
	}
	requireNoGrowth(t, before, "user stop, reset and delete of process and sub-agent handles")
}
