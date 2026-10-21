package native

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A descendant that keeps stdout or stderr open must not keep the managed process
// "running" after the shell has exited.

func waitProcessExited(t *testing.T, manager *ManagedProcessManager, id string, limit time.Duration) ProcessWaitResult {
	t.Helper()
	return manager.Wait(id, limit.Milliseconds())
}

func startDescendantTest(t *testing.T, command string) (*ManagedProcessManager, string, ProcessSnapshot) {
	t.Helper()
	workspace := t.TempDir()
	manager := NewProcessManager(workspace)
	t.Cleanup(func() { _ = manager.Close() })
	snapshot, err := manager.Start(processTestRequest(workspace, command, 1<<20))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return manager, snapshot.ID, snapshot
}

// killGroupOf kills the shell's process group, which holds the test's background sleep.
func killGroupOf(t *testing.T, pid int) {
	t.Helper()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func TestManagedProcessExitsWhileDescendantHoldsPipes(t *testing.T) {
	manager, id, started := startDescendantTest(t, "sleep 300 &")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	begun := time.Now()
	result := waitProcessExited(t, manager, id, 5*time.Second)
	if !result.Waited || result.Snapshot == nil || result.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = waited %v snapshot %+v, want exited", result.Waited, result.Snapshot)
	}
	if got := string(result.Snapshot.ExitCode); got != "0" {
		t.Fatalf("exit code = %q, want 0", got)
	}
	if elapsed := time.Since(begun); elapsed > 3*time.Second {
		t.Fatalf("exit recorded after %v, want about the %v delay", elapsed, bashDrainAfterExit)
	}
}

// pidGone reports whether the process is gone; a zombie counts as alive to kill -0, so
// /proc is read for it.
func pidGone(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && strings.Contains(string(data), ") Z ")
}

func TestManagedProcessKillsDescendantAfterExit(t *testing.T) {
	manager, id, started := startDescendantTest(t, "sleep 300 & echo $!")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	result := waitProcessExited(t, manager, id, 5*time.Second)
	if !result.Waited || result.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = waited %v snapshot %+v, want exited", result.Waited, result.Snapshot)
	}
	logs, _ := manager.SnapshotWithLogs(id, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false)
	pid, err := strconv.Atoi(strings.TrimSpace(logs.Output.text))
	if err != nil {
		t.Fatalf("descendant pid not in output %q: %v", logs.Output.text, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !pidGone(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !pidGone(pid) {
		t.Fatalf("descendant %d still alive after the exit was recorded", pid)
	}
}

func TestManagedProcessKillsTermIgnoringDescendantWithKill(t *testing.T) {
	manager, id, started := startDescendantTest(t, "(trap '' TERM; exec sleep 300) & echo $!")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	result := waitProcessExited(t, manager, id, 5*time.Second)
	if !result.Waited || result.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = waited %v state %v, want exited", result.Waited, result.Snapshot.State)
	}
	logs, _ := manager.SnapshotWithLogs(id, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false)
	pid, err := strconv.Atoi(strings.TrimSpace(logs.Output.text))
	if err != nil {
		t.Fatalf("descendant pid not in output %q: %v", logs.Output.text, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !pidGone(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !pidGone(pid) {
		t.Fatalf("TERM-ignoring descendant %d survived the grace and SIGKILL", pid)
	}
}

func TestManagedProcessKeepsOutputAndRealExitCodeWithDescendant(t *testing.T) {
	manager, id, started := startDescendantTest(t, "(sleep 300 &); echo hi; exit 3")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	result := waitProcessExited(t, manager, id, 5*time.Second)
	if !result.Waited || result.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = waited %v state %v, want exited", result.Waited, result.Snapshot.State)
	}
	if got := string(result.Snapshot.ExitCode); got != "3" {
		t.Fatalf("exit code = %q, want 3", got)
	}
	logs, _ := manager.SnapshotWithLogs(id, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false)
	if logs == nil || !strings.Contains(logs.Output.text, "hi") {
		t.Fatalf("output = %+v, want it to contain hi", logs)
	}
}

func TestManagedProcessRedirectedDescendantUnchanged(t *testing.T) {
	manager, id, started := startDescendantTest(t, "sleep 300 >/dev/null 2>&1 & echo done")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	result := waitProcessExited(t, manager, id, 5*time.Second)
	if !result.Waited || result.Snapshot.State != ProcessExited || string(result.Snapshot.ExitCode) != "0" {
		t.Fatalf("Wait() = waited %v snapshot %+v, want exited 0", result.Waited, result.Snapshot)
	}
}

func TestManagedProcessLongRunningStaysRunningAndStops(t *testing.T) {
	manager, id, started := startDescendantTest(t, "sleep 300")
	t.Cleanup(func() { killGroupOf(t, started.PID) })
	time.Sleep(2 * bashDrainAfterExit)
	snapshot, _ := manager.Snapshot(id)
	if snapshot.State != ProcessRunning {
		t.Fatalf("state after the delay = %v, want running", snapshot.State)
	}
	stopped, _ := manager.Stop(id, "test")
	if stopped.State != ProcessStopped {
		t.Fatalf("state after Stop = %v, want stopped", stopped.State)
	}
	if err := syscall.Kill(started.PID, 0); err == nil {
		t.Fatalf("process %s is still alive after Stop", strconv.Itoa(started.PID))
	}
}
