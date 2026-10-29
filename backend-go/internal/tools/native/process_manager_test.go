package native

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestStopAndStopOwnedKeepOneLockOrder is the order P6BM-08 established and the deadlock that
// broke the -race gate: recording a live manifest takes entry.mu and then manager.mu, so nothing
// may hold manager.mu while it takes entry.mu. stopOwned did exactly that while it inspected the
// state of each owned entry, so a Stop recording its manifest in between wedged both of them.
func TestStopAndStopOwnedKeepOneLockOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	manager := NewProcessManager(filepath.Join(workspace, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	request := processTestRequest(workspace, "sleep 30", 1024)
	request.Owner = "sub"
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	// Park the stop inside recordLiveManifest: from here on it holds entry.mu and has not taken
	// manager.mu yet. stopOwned then has to inspect the same entry, which is the interleaving.
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	manager.liveHook = func(string) {
		once.Do(func() { close(parked) })
		<-release
	}
	stopping := make(chan struct{})
	go func() {
		defer close(stopping)
		if _, found := manager.Stop(snapshot.ID, "test"); !found {
			t.Error("Stop did not find the process")
		}
	}()
	<-parked
	ownerDone := make(chan int, 1)
	go func() { ownerDone <- manager.stopOwned("sub", "test") }()
	// The pause only gives stopOwned time to reach the entry; the outcome does not depend on it.
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop and stopOwned deadlocked: entry.mu and manager.mu were taken in both orders")
	}
	select {
	case <-ownerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stopOwned never returned")
	}
}

// TestStartFailuresDoNotCarryTheStoragePath is P6BM-21: the model reads the start error, so it
// carries neither the tool storage directory nor the os cause; the log has both.
func TestStartFailuresDoNotCarryTheStoragePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	blocked := filepath.Join(workspace, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewProcessManager(filepath.Join(blocked, "processes"))
	_, err := manager.Start(processTestRequest(workspace, "true", 1024))
	if err == nil {
		t.Fatal("a start under a regular file must fail")
	}
	if !errors.Is(err, errProcessStorage) {
		t.Fatalf("err = %v, want errProcessStorage", err)
	}
	if strings.Contains(err.Error(), workspace) || strings.Contains(err.Error(), "processes") {
		t.Fatalf("the model-facing start error names the storage: %v", err)
	}
}

// TestRecoverAppliesRetentionWithoutAFinish is P6BM-24: retention used to run only when another
// process finished, so a profile that stopped starting processes kept every manifest and log.
func TestRecoverAppliesRetentionWithoutAFinish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	storage := filepath.Join(workspace, "processes")
	first := NewProcessManager(storage)
	request := processTestRequest(workspace, "true", 1024)
	var ids []string
	for index := 0; index < 3; index++ {
		snapshot, err := first.Start(request)
		if err != nil {
			t.Fatal(err)
		}
		if waited := first.Wait(snapshot.ID, 5000); !waited.Waited {
			t.Fatalf("process %d did not finish: %+v", index, waited)
		}
		ids = append(ids, snapshot.ID)
	}
	second := NewConfiguredProcessManager(sessionfs.NativeProcessSettings{StorageDir: storage, MaxCompleted: 1})
	if err := second.Recover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	kept := 0
	for _, id := range ids {
		if _, ok := second.Snapshot(id); ok {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("recovery kept %d of %d manifests, want max_completed 1", kept, len(ids))
	}
}

// TestProcessWaitPanicFailsTheProcessNotTheServer is P6BM-03 at the second site: the goroutine
// that waits for a started process and finishes it had no recover, so a panic in it took the
// server down and left the manifest saying running for ever. A nil command is the shortest
// panic: waitForProcess dereferences it.
func TestProcessWaitPanicFailsTheProcessNotTheServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	manager := NewProcessManager(filepath.Join(workspace, "processes"))
	id := "proc_panic"
	processDir := filepath.Join(manager.directory, id)
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatal(err)
	}
	limits := bashStreamLimits{headBytes: 3, tailBytes: 3}
	entry := &managedProcess{
		manifest:     ProcessManifest{SchemaVersion: 1, ID: id, State: ProcessRunning, StartedAt: processNow()},
		collector:    newBashOutputCollector(limits, limits, 100, false),
		stdout:       &bashSpillWriter{path: filepath.Join(processDir, "stdout.log"), capBytes: 1024},
		stderr:       &bashSpillWriter{path: filepath.Join(processDir, "stderr.log"), capBytes: 1024},
		done:         make(chan struct{}),
		killGrace:    250 * time.Millisecond,
		maxCompleted: 10,
	}
	manager.mu.Lock()
	manager.entries[id] = entry
	manager.manifests[id] = entry.manifest
	manager.mu.Unlock()
	manager.finishing.Add(1)
	manager.waitForProcess(nil, entry, id)
	manager.finishing.Wait()
	snapshot, ok := manager.Snapshot(id)
	if !ok || snapshot.State != ProcessFailed || snapshot.StopReason != "internal error" {
		t.Fatalf("snapshot = %+v, %v; want a process failed as internal error", snapshot, ok)
	}
	select {
	case <-entry.done:
	default:
		t.Fatal("the process was not released: done is still open")
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

// stopOnBeginAsync runs a Stop from inside Begin: the window P6BM-10 names, in which the
// handle is registered and the process does not exist yet, so the stop finds no pid and no
// command to signal.
type stopOnBeginAsync struct {
	manager *ManagedProcessManager
	called  bool
}

func (s *stopOnBeginAsync) Begin(id string) error {
	s.called = true
	_, _ = s.manager.Stop(id, "stopped_by_agent")
	return nil
}

func (s *stopOnBeginAsync) Abort(string, error) {}

func (s *stopOnBeginAsync) End(ProcessSnapshot) {}

func TestStopBeforeTheProcessStartsStillStopsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	stop := &stopOnBeginAsync{manager: manager}
	request := processTestRequest(directory, "sleep 300", 1024)
	request.Policy.KillGraceMs = 200
	request.Async = stop
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	if !stop.called {
		t.Fatal("the test never reached the window between Begin and command.Start")
	}
	if snapshot.State != ProcessStopped && snapshot.State != ProcessExited {
		t.Fatalf("a stop that arrived before the process started was lost: state=%q left_running=%t", snapshot.State, snapshot.LeftRunning)
	}
	if snapshot.LeftRunning {
		t.Fatal("the process is recorded as left running, but the stop only arrived early")
	}
	manager.mu.Lock()
	recorded := manager.manifests[snapshot.ID]
	manager.mu.Unlock()
	if recorded.State == ProcessRunning || recorded.LeftRunning {
		t.Fatalf("the recorded manifest says state=%q left_running=%t", recorded.State, recorded.LeftRunning)
	}
	// The process itself is gone: the group the shell led has no member left.
	if snapshot.PID > 0 && processGroupAlive(snapshot.PID) && !waitGroupGone(snapshot.PID) {
		t.Fatalf("process group %d of the stopped process is still alive", snapshot.PID)
	}
}

func waitGroupGone(pgid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pgid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestCloseWaitsForThePruneOfAFinishedProcess is P6BM-09: a finisher closes done before it
// prunes, so Close must wait for the finisher too, else a caller that removes the storage
// folder right after Close races the removal still in flight.
func TestCloseWaitsForThePruneOfAFinishedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	removing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	manager.removeAll = func(path string) error {
		once.Do(func() { close(removing) })
		<-release
		return os.RemoveAll(path)
	}
	request := processTestRequest(directory, "exit 0", 1024)
	// Every completed process is pruned at once, so the finisher of this one removes its
	// own folder and can be held there.
	request.Policy.MaxCompleted = 0
	snapshot, err := manager.Start(request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-removing:
	case <-time.After(5 * time.Second):
		t.Fatal("the finished process was never pruned")
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the finisher was still removing a folder", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the removal finished")
	}
	if _, err := os.Stat(filepath.Join(directory, "processes", snapshot.ID)); !os.IsNotExist(err) {
		t.Fatalf("the pruned folder still exists after Close: %v", err)
	}
}

// TestCloseSignalsEveryRunningProcessAtOnce pins P6BM-16: a stop may wait kill_grace_ms for
// SIGTERM and again for SIGKILL, so children that ignore SIGTERM must cost one grace together
// and not one grace each. Serial stops would need children x grace here.
func TestCloseSignalsEveryRunningProcessAtOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	const children, grace = 6, 500 * time.Millisecond
	request := processTestRequest(directory, "trap '' TERM; sleep 30", 1024)
	request.Policy.KillGraceMs = int64(grace / time.Millisecond)
	request.Policy.MaxRunning = children
	ids := make([]string, 0, children)
	for range children {
		snapshot, err := manager.Start(request)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, snapshot.ID)
	}
	started := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if took, limit := time.Since(started), 2*grace; took > limit {
		t.Fatalf("Close took %v for %d children that ignore SIGTERM at a %v grace, want at most %v", took, children, grace, limit)
	}
	for _, id := range ids {
		snapshot, found := manager.Snapshot(id)
		if !found || snapshot == nil || snapshot.State != ProcessStopped {
			t.Fatalf("process %s after Close = %#v, want stopped", id, snapshot)
		}
	}
}
