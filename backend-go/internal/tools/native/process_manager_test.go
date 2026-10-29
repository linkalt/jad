package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestProcessManagerPersistsBoundedLogsAndRecoversManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	request := processTestRequest(directory, "printf 'abcdefghijklmno'", 10)
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	waited := manager.Wait(snapshot.ID, 3000)
	if !waited.Found || !waited.Waited || waited.Snapshot == nil {
		t.Fatalf("Wait() = %#v, want completed process", waited)
	}
	if waited.Snapshot.State != ProcessExited {
		t.Fatalf("state = %q, want exited", waited.Snapshot.State)
	}
	if got := waited.Snapshot.Output.stdout.producedBytes; got != 15 {
		t.Fatalf("live stdout produced bytes = %d, want 15", got)
	}
	if !strings.Contains(waited.Snapshot.Output.text, "9 bytes omitted") || !strings.Contains(waited.Snapshot.Output.text, "abc") || !strings.Contains(waited.Snapshot.Output.text, "mno") {
		t.Fatalf("live output = %q, want bounded head/tail and omitted-byte marker", waited.Snapshot.Output.text)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	recovered := NewProcessManager(filepath.Join(directory, "processes"))
	if err := recovered.Recover(); err != nil {
		t.Fatal(err)
	}
	fromDisk, found := recovered.SnapshotWithLogs(
		snapshot.ID,
		ProcessStreamLimits{HeadBytes: 3, TailBytes: 3},
		ProcessStreamLimits{HeadBytes: 3, TailBytes: 3},
		100,
		false,
	)
	if !found || fromDisk == nil {
		t.Fatal("recovered process was not found")
	}
	if fromDisk.StdoutStoredBytes != 10 || !fromDisk.StdoutCapped {
		t.Fatalf("recovered log metadata = stored %d, capped %v; want 10, true", fromDisk.StdoutStoredBytes, fromDisk.StdoutCapped)
	}
	if got := fromDisk.Output.stdout.producedBytes; got != 15 {
		t.Fatalf("recovered stdout produced bytes = %d, want 15", got)
	}
	if !strings.Contains(fromDisk.Output.text, "9 bytes omitted") || !strings.Contains(fromDisk.Output.text, "abc") || !strings.Contains(fromDisk.Output.text, "hij") {
		t.Fatalf("recovered bounded output = %q, want head/tail and omitted-byte marker", fromDisk.Output.text)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessManagerWaitTimeoutAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	snapshot, err := manager.Start(processTestRequest(directory, "sleep 30", 1024))
	if err != nil {
		t.Fatal(err)
	}
	waited := manager.Wait(snapshot.ID, 25)
	if !waited.Found || waited.Waited || waited.Snapshot == nil || waited.Snapshot.State != ProcessRunning {
		t.Fatalf("timed Wait() = %#v, want running process with waited=false", waited)
	}
	stopped, found := manager.Stop(snapshot.ID, "stopped_by_agent")
	if !found || stopped == nil {
		t.Fatal("Stop() did not find the process")
	}
	if stopped.State != ProcessStopped || stopped.StopReason != "stopped_by_agent" {
		t.Fatalf("stopped snapshot = state %q reason %q, want stopped/stopped_by_agent", stopped.State, stopped.StopReason)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessManagerRecoveryMarksRunningManifestLost(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "processes")
	id := "proc_recovered"
	processDirectory := filepath.Join(directory, id)
	if err := os.MkdirAll(processDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := ProcessManifest{
		SchemaVersion: 1,
		ID:            id,
		State:         ProcessRunning,
		CWD:           "/workspace",
		StartedAt:     "2025-01-01T00:00:00.000Z",
		PID:           1234,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDirectory, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewProcessManager(directory)
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	snapshot, found := manager.Snapshot(id)
	if !found || snapshot == nil {
		t.Fatal("recovered manifest was not found")
	}
	if snapshot.State != ProcessLost || snapshot.StopReason != "backend_restarted" || snapshot.EndedAt == "" {
		t.Fatalf("recovered snapshot = %#v, want lost/backend_restarted with ended_at", snapshot)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessManagerPrunesOldestCompletedProcesses(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "processes")
	manifests := []ProcessManifest{
		{SchemaVersion: 1, ID: "proc_old", State: ProcessExited, CWD: "/workspace", StartedAt: "2025-01-01T00:00:00.000Z", EndedAt: "2025-01-01T00:00:01.000Z"},
		{SchemaVersion: 1, ID: "proc_new", State: ProcessExited, CWD: "/workspace", StartedAt: "2025-01-02T00:00:00.000Z", EndedAt: "2025-01-02T00:00:01.000Z"},
	}
	for _, manifest := range manifests {
		processDirectory := filepath.Join(directory, manifest.ID)
		if err := os.MkdirAll(processDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(processDirectory, "manifest.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewProcessManager(directory)
	if err := manager.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := manager.prune(1, 0); err != nil {
		t.Fatal(err)
	}
	processes := manager.List("")
	if len(processes) != 1 || processes[0].ID != "proc_new" {
		t.Fatalf("List() after prune = %#v, want only proc_new", processes)
	}
	if _, err := os.Stat(filepath.Join(directory, "proc_old")); !os.IsNotExist(err) {
		t.Fatalf("old process directory still exists, stat error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func processTestRequest(workspace, command string, logCap int64) ProcessStartRequest {
	return ProcessStartRequest{
		CWD:         workspace,
		DisplayCWD:  workspace,
		Command:     command,
		Environment: os.Environ(),
		APIKeyEnv:   "OPENAI_API_KEY",
		Policy: sessionfs.NativeProcessSettings{
			Shell:         "/bin/sh",
			KillGraceMs:   250,
			MaxRunning:    4,
			MaxCompleted:  10,
			RetentionDays: 0,
			Stdout:        sessionfs.NativeBashStreamBudget{HeadBytes: 3, TailBytes: 3},
			Stderr:        sessionfs.NativeBashStreamBudget{HeadBytes: 3, TailBytes: 3},
			LogCapBytes:   logCap,
			MaxLineChars:  100,
			StdinMaxBytes: 1024,
			Env:           map[string]string{"NO_COLOR": "1"},
			EnvDeny:       []string{},
		},
	}
}

func TestBuildNativeProcessEnvironmentKeepsOnlyAllowlistedVariables(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin", "HOME=/tmp", "FIXTURE_SECRET=ambient-secret", "UNLISTED_SECRET=do-not-pass",
		"JAD_API_TOKEN=server-secret", "DENIED_BY_SETTING=denied",
	}
	overrides := map[string]string{"NO_COLOR": "1", "FROM_ENV_FILE": "configured", "DENIED_BY_SETTING": "configured"}
	environment := buildNativeProcessEnvironment(base, overrides, "FIXTURE_SECRET", []string{"DENIED_BY_SETTING"})
	values := make(map[string]string)
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		values[name] = value
	}
	want := map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/tmp", "NO_COLOR": "1", "FROM_ENV_FILE": "configured"}
	if len(values) != len(want) {
		t.Fatalf("environment = %v, want %v", values, want)
	}
	for name, value := range want {
		if values[name] != value {
			t.Fatalf("environment = %v, want %v", values, want)
		}
	}
}

func TestProcessChildDoesNotInheritServerSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	t.Setenv("PROCESS_CANARY_SECRET", "canary-value")
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()
	snapshot, err := manager.Start(processTestRequest(directory, `printf 'canary=[%s] path=[%s]' "$PROCESS_CANARY_SECRET" "$PATH"`, 1<<16))
	if err != nil {
		t.Fatal(err)
	}
	waited := manager.Wait(snapshot.ID, 3000)
	if !waited.Found || waited.Snapshot == nil || waited.Snapshot.State != ProcessExited {
		t.Fatalf("Wait() = %#v, want an exited process", waited)
	}
	output, err := os.ReadFile(filepath.Join(directory, "processes", snapshot.ID, "stdout.log"))
	if err != nil {
		entries, _ := os.ReadDir(filepath.Join(directory, "processes", snapshot.ID))
		t.Fatalf("read stdout log: %v (entries %v)", err, entries)
	}
	if strings.Contains(string(output), "canary-value") || !strings.Contains(string(output), "canary=[]") {
		t.Fatalf("child output = %q, want the canary variable hidden", output)
	}
	if strings.Contains(string(output), "path=[]") {
		t.Fatalf("child output = %q, want PATH kept", output)
	}
}

// TestStopKeepsTheTerminalManifestOfAProcessThatExitsFirst is P6BM-08: a stop that passed
// its state check before the process exited must not write "running" over the terminal
// manifest finish already recorded, in memory or on disk.
func TestStopKeepsTheTerminalManifestOfAProcessThatExitsFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	assertTerminal := func(when, id string) {
		t.Helper()
		manager.mu.Lock()
		recorded := manager.manifests[id]
		manager.mu.Unlock()
		if recorded.State == ProcessRunning || recorded.LeftRunning {
			t.Fatalf("%s: the recorded manifest says state=%q left_running=%t", when, recorded.State, recorded.LeftRunning)
		}
		fromDisk, _ := manager.readManifest(id)
		if fromDisk.State == ProcessRunning {
			t.Fatalf("%s: the manifest on disk says running", when)
		}
	}

	// The guarded write itself: a stale "running" clone of a finished entry is dropped.
	snapshot, err := manager.Start(processTestRequest(directory, "true", 1024))
	if err != nil {
		t.Fatal(err)
	}
	if waited := manager.Wait(snapshot.ID, 5000); !waited.Waited || waited.Snapshot.State != ProcessExited {
		t.Fatalf("process = %#v, want it exited", waited.Snapshot)
	}
	manager.mu.Lock()
	entry := manager.entries[snapshot.ID]
	manager.mu.Unlock()
	if entry == nil {
		t.Fatalf("no entry for %s", snapshot.ID)
	}
	entry.mu.Lock()
	stale := cloneProcessManifest(entry.manifest)
	entry.mu.Unlock()
	stale.State = ProcessRunning
	stale.LeftRunning = true
	stale.StopReason = "stopped_by_agent"
	entry.mu.Lock()
	manager.recordLiveManifest(entry, stale)
	entry.mu.Unlock()
	assertTerminal("after a stale write", snapshot.ID)

	// The same through the public path: a stop racing a process that exits at once.
	for round := 0; round < 20; round++ {
		started, startErr := manager.Start(processTestRequest(directory, "true", 1024))
		if startErr != nil {
			t.Fatal(startErr)
		}
		var stopped sync.WaitGroup
		stopped.Add(1)
		go func(id string) {
			defer stopped.Done()
			_, _ = manager.Stop(id, "stopped_by_agent")
		}(started.ID)
		manager.Wait(started.ID, 5000)
		stopped.Wait()
		assertTerminal("after a stop racing the exit", started.ID)
	}
}
