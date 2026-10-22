package native

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

// These tests pin process behaviour that the async manager work must keep. They assert
// on manager state, not on tool parameter names.

func TestProcessCharacterizationNonZeroExitIsExitedWithItsCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()
	started, err := manager.Start(processTestRequest(directory, "exit 3", 1024))
	if err != nil {
		t.Fatal(err)
	}
	waited := manager.Wait(started.ID, 5000)
	if !waited.Found || !waited.Waited || waited.Snapshot == nil {
		t.Fatalf("Wait() = %#v, want a finished process", waited)
	}
	if waited.Snapshot.State != ProcessExited || string(waited.Snapshot.ExitCode) != "3" {
		t.Fatalf("state %q exit code %s, want exited with 3", waited.Snapshot.State, waited.Snapshot.ExitCode)
	}
}

func TestProcessCharacterizationStopIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()
	started, err := manager.Start(processTestRequest(directory, "sleep 30", 1024))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		stopped, found := manager.Stop(started.ID, "stopped_by_agent")
		if !found || stopped == nil || stopped.State != ProcessStopped || stopped.StopReason != "stopped_by_agent" {
			t.Fatalf("stop %d = %+v found=%v, want stopped/stopped_by_agent", attempt, stopped, found)
		}
	}
	if _, found := manager.Stop("proc_unknown", "stopped_by_agent"); found {
		t.Fatal("Stop() of an unknown id reported found")
	}
}

// A process started by a tool call goes on after the context of that call ends.
func TestProcessCharacterizationSurvivesTheCallThatStartedIt(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	output, err := executeNativeProcess(ctx, map[string]any{"command": "sleep 30"}, tools.ExecutionContext{
		CallID: "call-start", WorkspaceRoot: runner.workspace, SessionDir: runner.workspace,
	}, runner.settings, runner.manager, "")
	if err != nil {
		t.Fatal(err)
	}
	id := processIDOf(t, output)
	cancel()
	time.Sleep(150 * time.Millisecond)
	snapshot, found := runner.manager.Snapshot(id)
	if !found || snapshot.State != ProcessRunning {
		t.Fatalf("process after the call context ended = %+v found=%v, want running", snapshot, found)
	}
	runner.manager.Stop(id, "stopped_by_agent")
}

// TestProcessCharacterizationSessionProcessesHaveNoOwnerWP4Flips documents that a
// process started for the session itself carries no owner, so no owner-based stop can
// reach it. The async work stamps the session key here and this test is then rewritten.
func TestProcessCharacterizationSessionProcessesHaveNoOwnerWP4Flips(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	id := processIDOf(t, runner.run(map[string]any{"command": "sleep 30"}))
	owner, found := runner.manager.ownerOf(id)
	if !found || owner != "" {
		t.Fatalf("ownerOf() = %q found=%v, want an empty owner", owner, found)
	}
	if stopped := runner.manager.stopOwned("", "x"); stopped != 0 {
		t.Fatalf("stopOwned(\"\") stopped %d processes, want 0", stopped)
	}
	runner.manager.Stop(id, "stopped_by_agent")
}
