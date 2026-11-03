package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/sessionfs"
)

type ProcessState string

const (
	ProcessRunning ProcessState = "running"
	ProcessExited  ProcessState = "exited"
	ProcessStopped ProcessState = "stopped"
	ProcessFailed  ProcessState = "failed"
	ProcessLost    ProcessState = "lost"
)

type ProcessManifest struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	State         ProcessState `json:"state"`
	CWD           string       `json:"cwd"`
	StartedAt     string       `json:"started_at"`
	PID           int          `json:"pid,omitempty"`
	// The remaining fields follow the order in which the TypeScript manager
	// assigns them (the model sees this JSON in process results): a stop reason
	// and left_running are set while the process is stopping, the exit status and
	// ended_at when it finishes. A process recovered as "lost" after a restart is
	// the one case TypeScript orders differently (ended_at, then stop_reason);
	// endedBeforeStop records that and process_manifest_order.go honours it.
	StopReason          string          `json:"stop_reason,omitempty"`
	LeftRunning         bool            `json:"left_running,omitempty"`
	ExitCode            json.RawMessage `json:"exit_code,omitempty"`
	Signal              json.RawMessage `json:"signal,omitempty"`
	EndedAt             string          `json:"ended_at,omitempty"`
	StdoutProducedBytes json.RawMessage `json:"stdout_produced_bytes,omitempty"`
	StderrProducedBytes json.RawMessage `json:"stderr_produced_bytes,omitempty"`
	StdoutStoredBytes   json.RawMessage `json:"stdout_stored_bytes,omitempty"`
	StderrStoredBytes   json.RawMessage `json:"stderr_stored_bytes,omitempty"`
	StdoutCapped        json.RawMessage `json:"stdout_capped,omitempty"`
	StderrCapped        json.RawMessage `json:"stderr_capped,omitempty"`

	// endedBeforeStop is set for a manifest whose ended_at precedes stop_reason:
	// a running process recovered as lost without a prior stop reason.
	endedBeforeStop bool
}

type ProcessSnapshot struct {
	ProcessManifest
	Output            bashRenderedOutput `json:"-"`
	StdoutStoredBytes int64              `json:"stdout_stored_bytes"`
	StderrStoredBytes int64              `json:"stderr_stored_bytes"`
	StdoutCapped      bool               `json:"stdout_capped"`
	StderrCapped      bool               `json:"stderr_capped"`
}

// ProcessStreamLimits bounds the head and tail of a persisted process log
// returned by SnapshotWithLogs.
type ProcessStreamLimits struct {
	HeadBytes int
	TailBytes int
}

type ProcessStartRequest struct {
	CWD        string
	DisplayCWD string
	Command    string
	// Stdin is written once to the process's standard input, which is then
	// closed. It is never stored. Empty leaves the input closed from the start.
	Stdin       string
	Policy      sessionfs.NativeProcessSettings
	Environment []string
	APIKeyEnv   string
	// Owner names who started the process (a sub-agent run id); empty for the session
	// itself. It is kept in memory only, never in the manifest the model sees.
	Owner string
	// OwnerLimit, with Owner, refuses a start while Owner already has that many
	// processes running; zero is no limit. The count and the start happen under one lock.
	OwnerLimit int
	// Async, when set, is told about the process as a handle of the async manager: Begin
	// before the process exists (an error refuses the start), End once it has ended.
	Async ProcessAsync
}

// ProcessAsync is how a process reports itself to the async manager. End runs before the
// done channel closes, so a caller that saw the process end knows the signal is queued.
type ProcessAsync interface {
	Begin(id string) error
	End(ProcessSnapshot)
	// Abort ends a handle whose process never started; the start call reports the error.
	Abort(id string, cause error)
}

type ProcessWaitResult struct {
	Snapshot *ProcessSnapshot
	Found    bool
	Waited   bool
}

type contextWaiter interface {
	WaitContext(ctx context.Context, id string, timeoutMs int64) ProcessWaitResult
}

// waitProcess waits through WaitContext when the manager has it, so a cancelled call does
// not leave a waiter behind; otherwise it falls back to Wait.
func waitProcess(ctx context.Context, manager ProcessManager, id string, timeoutMs int64) ProcessWaitResult {
	if waiter, ok := manager.(contextWaiter); ok {
		return waiter.WaitContext(ctx, id, timeoutMs)
	}
	return manager.Wait(id, timeoutMs)
}

// ProcessManager is the managed long-running process boundary injected into
// native.process tools and owned by the application resolver.
type ProcessManager interface {
	Recover() error
	Start(ProcessStartRequest) (ProcessSnapshot, error)
	SnapshotWithLogs(id string, stdoutLimits, stderrLimits ProcessStreamLimits, maxLineChars int, stripANSI bool) (*ProcessSnapshot, bool)
	List(filter string) []ProcessSnapshot
	Wait(id string, timeoutMs int64) ProcessWaitResult
	Stop(id, reason string) (*ProcessSnapshot, bool)
	Close() error
}

type ManagedProcessManager struct {
	directory string

	mu        sync.Mutex
	entries   map[string]*managedProcess
	manifests map[string]ProcessManifest
	closed    bool
	starting  sync.WaitGroup
	// finishing counts the goroutines that finish a started process (waitForProcess and
	// the prune it ends with): Close waits for them so it does not return while one is
	// still writing a manifest or removing a directory.
	finishing sync.WaitGroup
	persistMu sync.Mutex
	// removeAll deletes the folder of a pruned process; it is a field so a test can hold
	// the finisher inside the prune that follows the close of its done channel.
	removeAll func(string) error
	// liveHook, when set, is called by recordLiveManifest with entry.mu held and manager.mu not
	// yet taken, so a test can park a stop at exactly the point where the two locks are taken.
	// It is nil in production.
	liveHook  func(id string)
	recoverMu sync.Mutex
	recovered bool
	// stopOrphans and orphanGrace are the policy Recover applies to a process a crashed
	// server left running; see SetOrphanPolicy. Guarded by mu.
	stopOrphans bool
	orphanGrace time.Duration
	// maxCompleted and retentionDays are the retention Recover applies to the manifests it
	// reads back; see SetRetention. Guarded by mu.
	maxCompleted  int64
	retentionDays int64
}

type managedProcess struct {
	mu       sync.Mutex
	manifest ProcessManifest
	command  *exec.Cmd
	// stdinWriter is the write end of the stdin pipe when the start had stdin text.
	// It is closed when the process ends so a writer blocked on a process that
	// never reads cannot outlive it.
	stdinWriter   *os.File
	collector     *bashOutputCollector
	stdout        *bashSpillWriter
	stderr        *bashSpillWriter
	done          chan struct{}
	finishOnce    sync.Once
	finalizeErr   error
	killGrace     time.Duration
	maxCompleted  int64
	retentionDays int64
	owner         string // see ProcessStartRequest.Owner; never changes after the start
	async         ProcessAsync
}

type processStreamWriter struct {
	entry  *managedProcess
	stream bashStream
}

// NewConfiguredProcessManager is NewProcessManager with the orphan policy and the retention of
// settings.
func NewConfiguredProcessManager(settings sessionfs.NativeProcessSettings) *ManagedProcessManager {
	manager := NewProcessManager(settings.StorageDir)
	manager.SetOrphanPolicy(settings.StopOrphans, time.Duration(settings.KillGraceMs)*time.Millisecond)
	manager.SetRetention(settings.MaxCompleted, settings.RetentionDays)
	return manager
}

// SetRetention records the retention of the process tool, so Recover can apply it to the
// manifests it reads back instead of waiting for another process to finish (P6BM-24).
func (manager *ManagedProcessManager) SetRetention(maxCompleted, retentionDays int64) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.maxCompleted, manager.retentionDays = maxCompleted, retentionDays
}

func NewProcessManager(directory string) *ManagedProcessManager {
	return &ManagedProcessManager{
		directory: filepath.Clean(directory),
		entries:   make(map[string]*managedProcess),
		manifests: make(map[string]ProcessManifest),
		removeAll: os.RemoveAll,
	}
}

func (writer processStreamWriter) Write(chunk []byte) (int, error) {
	if writer.entry == nil || len(chunk) == 0 {
		return len(chunk), nil
	}
	writer.entry.mu.Lock()
	defer writer.entry.mu.Unlock()
	writer.entry.collector.push(writer.stream, chunk)
	sink := writer.entry.stdout
	if writer.stream == bashStderr {
		sink = writer.entry.stderr
	}
	if sink != nil {
		sink.write(chunk)
	}
	return len(chunk), nil
}

// errProcessManagerClosed says the manager was closed, so a caller can tell a shutdown from an
// unusable storage directory.
var errProcessManagerClosed = errors.New("process manager is closed")

// errProcessStorage is what a failed start reports to the model. The os error names the tool
// storage directory, which is server detail: it is logged, never rendered (P6BM-21).
var errProcessStorage = errors.New("the process storage is not usable; the server log names the path and the cause")

func (manager *ManagedProcessManager) Start(request ProcessStartRequest) (ProcessSnapshot, error) {
	if runtime.GOOS == "windows" {
		return ProcessSnapshot{}, errors.New("managed process groups require a POSIX platform")
	}
	if request.Command == "" {
		return ProcessSnapshot{}, errors.New("process command is empty")
	}
	if err := manager.Recover(); err != nil {
		if errors.Is(err, errProcessManagerClosed) {
			return ProcessSnapshot{}, err
		}
		slog.Error("process recovery failed", "error", err)
		return ProcessSnapshot{}, errProcessStorage
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return ProcessSnapshot{}, errProcessManagerClosed
	}
	running := 0
	for _, manifest := range manager.manifests {
		if manifest.State == ProcessRunning {
			running++
		}
	}
	if int64(running) >= request.Policy.MaxRunning {
		manager.mu.Unlock()
		return ProcessSnapshot{}, errors.New("process capacity reached")
	}
	if request.Owner != "" && request.OwnerLimit > 0 && manager.runningOwnedLocked(request.Owner) >= request.OwnerLimit {
		manager.mu.Unlock()
		return ProcessSnapshot{}, fmt.Errorf("this sub-agent already has %d processes running, its limit; stop one first", request.OwnerLimit)
	}
	id, err := newManagedProcessID()
	if err != nil {
		manager.mu.Unlock()
		return ProcessSnapshot{}, fmt.Errorf("create process id: %w", err)
	}
	manifest := ProcessManifest{
		SchemaVersion: 1,
		ID:            id,
		State:         ProcessRunning,
		CWD:           request.DisplayCWD,
		StartedAt:     processNow(),
	}
	processDir := filepath.Join(manager.directory, id)
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		manager.mu.Unlock()
		slog.Error("process storage could not be created", "process", id, "error", err)
		return ProcessSnapshot{}, errProcessStorage
	}
	collector := newBashOutputCollector(
		bashStreamLimits{headBytes: int(request.Policy.Stdout.HeadBytes), tailBytes: int(request.Policy.Stdout.TailBytes)},
		bashStreamLimits{headBytes: int(request.Policy.Stderr.HeadBytes), tailBytes: int(request.Policy.Stderr.TailBytes)},
		int(request.Policy.MaxLineChars),
		request.Policy.StripANSI,
	)
	entry := &managedProcess{
		manifest:      manifest,
		collector:     collector,
		stdout:        &bashSpillWriter{path: filepath.Join(processDir, "stdout.log"), capBytes: request.Policy.LogCapBytes},
		stderr:        &bashSpillWriter{path: filepath.Join(processDir, "stderr.log"), capBytes: request.Policy.LogCapBytes},
		done:          make(chan struct{}),
		killGrace:     time.Duration(request.Policy.KillGraceMs) * time.Millisecond,
		maxCompleted:  request.Policy.MaxCompleted,
		retentionDays: request.Policy.RetentionDays,
		owner:         request.Owner,
		async:         request.Async,
	}
	manager.entries[id] = entry
	manager.manifests[id] = manifest
	manager.starting.Add(1)
	manager.mu.Unlock()
	defer manager.starting.Done()

	if request.Async != nil {
		if err := request.Async.Begin(id); err != nil {
			manager.removeEntry(id)
			_ = os.RemoveAll(processDir)
			return ProcessSnapshot{}, err
		}
	}

	// Name this server before the manifest says running, so another server scanning
	// the directory never sees a running process without an owner. The process's own
	// start time is added once it exists (below); until then it is never signalled.
	manager.writeOwnerRecord(id, 0)
	if err := manager.persistManifest(manifest); err != nil {
		manager.removeEntry(id)
		_ = os.RemoveAll(processDir)
		slog.Error("process manifest could not be persisted", "process", id, "error", err)
		if request.Async != nil {
			request.Async.Abort(id, errProcessStorage)
		}
		return ProcessSnapshot{}, errProcessStorage
	}
	environment := buildNativeProcessEnvironment(request.Environment, request.Policy.Env, request.APIKeyEnv, request.Policy.EnvDeny)
	resolvedShell, err := resolveBashExecutable(request.Policy.Shell, environment, request.CWD)
	if err != nil {
		manager.failProcess(entry, err.Error())
		return manager.snapshotEntry(entry), nil
	}
	arguments := []string{"-c", request.Command}
	if request.Policy.LoginShell {
		arguments = []string{"-l", "-c", request.Command}
	}
	command := newInterpreterCommand(request.Policy.Shell, resolvedShell, arguments...)
	command.Dir = request.CWD
	command.Env = environment
	command.Stdout = processStreamWriter{entry: entry, stream: bashStdout}
	command.Stderr = processStreamWriter{entry: entry, stream: bashStderr}
	// An own pipe, not exec's copier: Wait must never block on a descendant that
	// keeps the read end open and never reads it.
	var stdinReader, stdinWriter *os.File
	if request.Stdin != "" {
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			manager.failProcess(entry, pipeErr.Error())
			return manager.snapshotEntry(entry), nil
		}
		command.Stdin = reader
		stdinReader, stdinWriter = reader, writer
	}
	// Wait returns this long after the shell exits even if a descendant still holds
	// stdout or stderr; output captured so far is kept.
	command.WaitDelay = processDrainAfterExit(request.Policy.DrainAfterExitMs)
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		if stdinReader != nil {
			_ = stdinReader.Close()
			_ = stdinWriter.Close()
		}
		manager.failProcess(entry, err.Error())
		return manager.snapshotEntry(entry), nil
	}
	if stdinReader != nil {
		// The child owns the read end now. A program that exits or never reads its
		// input makes the write fail (EPIPE); that is its choice, not a failed start.
		_ = stdinReader.Close()
		go func(writer *os.File, text string) {
			_, _ = io.WriteString(writer, text)
			_ = writer.Close()
		}(stdinWriter, request.Stdin)
	}
	entry.mu.Lock()
	entry.stdinWriter = stdinWriter
	entry.command = command
	entry.manifest.PID = command.Process.Pid
	// A stop that arrived before the process existed could not signal it: it only set the
	// reason and reported the process left running. The process exists from here on.
	stopped := entry.manifest.StopReason != ""
	if stopped {
		entry.manifest.LeftRunning = false
	}
	manifest = cloneProcessManifest(entry.manifest)
	entry.mu.Unlock()
	manager.finishing.Add(1)
	go manager.waitForProcess(command, entry, id)
	manager.writeOwnerRecord(id, manifest.PID)
	// The running manifest goes through the guarded write: the finisher may already have written
	// the terminal one, and a stale "running" clone landing on disk after it is exactly the
	// phantom P6BM-08 is about (the map is guarded by finish, the file was not).
	entry.mu.Lock()
	persistErr := manager.recordLiveManifest(entry, manifest)
	entry.mu.Unlock()
	if persistErr != nil {
		manager.signalProcess(entry, true)
		_ = waitForManagedProcess(entry, 2*time.Second)
		slog.Error("running process manifest could not be persisted", "process", id, "error", persistErr)
		return ProcessSnapshot{}, errProcessStorage
	}
	if stopped {
		// The stop that arrived before this process existed is carried out now, so the window
		// between Begin and Start cannot lose it.
		manager.stopSignalled(entry)
	}
	return manager.snapshotEntry(entry), nil
}

func (manager *ManagedProcessManager) SnapshotWithLogs(id string, stdoutLimits, stderrLimits ProcessStreamLimits, maxLineChars int, stripANSI bool) (*ProcessSnapshot, bool) {
	stdoutWindow := bashStreamLimits{headBytes: stdoutLimits.HeadBytes, tailBytes: stdoutLimits.TailBytes}
	stderrWindow := bashStreamLimits{headBytes: stderrLimits.HeadBytes, tailBytes: stderrLimits.TailBytes}
	manager.mu.Lock()
	entry := manager.entries[id]
	manifest, exists := manager.manifests[id]
	manager.mu.Unlock()
	if entry != nil {
		snapshot := manager.snapshotEntry(entry)
		return &snapshot, true
	}
	if !exists {
		return nil, false
	}
	snapshot := processSnapshotFromManifest(manifest)
	stdout, stdoutBytes := manager.readWindows(id, "stdout.log", stdoutWindow)
	stderr, stderrBytes := manager.readWindows(id, "stderr.log", stderrWindow)
	collector := newBashOutputCollector(stdoutWindow, stderrWindow, maxLineChars, stripANSI)
	for _, chunk := range stdout {
		collector.push(bashStdout, chunk)
	}
	for _, chunk := range stderr {
		collector.push(bashStderr, chunk)
	}
	collector.end()
	stdoutProduced := stdoutBytes
	if len(manifest.StdoutProducedBytes) > 0 {
		stdoutProduced = rawJSONInt64(manifest.StdoutProducedBytes, stdoutBytes)
	}
	stderrProduced := stderrBytes
	if len(manifest.StderrProducedBytes) > 0 {
		stderrProduced = rawJSONInt64(manifest.StderrProducedBytes, stderrBytes)
	}
	snapshot.Output = collector.renderWithProducedBytes(stdoutProduced, stderrProduced)
	return &snapshot, true
}

func (manager *ManagedProcessManager) List(filter string) []ProcessSnapshot {
	manager.mu.Lock()
	manifests := make([]ProcessManifest, 0, len(manager.manifests))
	entries := make(map[string]*managedProcess, len(manager.entries))
	for _, manifest := range manager.manifests {
		if filter == "" || string(manifest.State) == filter {
			manifests = append(manifests, cloneProcessManifest(manifest))
		}
	}
	for id, entry := range manager.entries {
		entries[id] = entry
	}
	manager.mu.Unlock()
	snapshots := make([]ProcessSnapshot, 0, len(manifests))
	for _, manifest := range manifests {
		if entry := entries[manifest.ID]; entry != nil {
			snapshots = append(snapshots, manager.snapshotEntry(entry))
		} else {
			snapshots = append(snapshots, processSnapshotFromManifest(manifest))
		}
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].StartedAt > snapshots[j].StartedAt
	})
	return snapshots
}

func (manager *ManagedProcessManager) Wait(id string, timeoutMs int64) ProcessWaitResult {
	return manager.WaitContext(context.Background(), id, timeoutMs)
}

// WaitContext is Wait that also returns when ctx ends (the process keeps running).
func (manager *ManagedProcessManager) WaitContext(ctx context.Context, id string, timeoutMs int64) ProcessWaitResult {
	manager.mu.Lock()
	entry := manager.entries[id]
	_, exists := manager.manifests[id]
	manager.mu.Unlock()
	if entry == nil {
		if !exists {
			return ProcessWaitResult{}
		}
		snapshot, found := manager.SnapshotWithLogs(id, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false)
		return ProcessWaitResult{Snapshot: snapshot, Found: found, Waited: true}
	}
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	waited := false
	select {
	case <-entry.done:
		waited = true
	case <-timer.C:
	case <-ctx.Done():
	}
	snapshot, found := manager.SnapshotWithLogs(id, ProcessStreamLimits{}, ProcessStreamLimits{}, 0, false)
	return ProcessWaitResult{Snapshot: snapshot, Found: found, Waited: waited}
}

func (manager *ManagedProcessManager) Stop(id, reason string) (*ProcessSnapshot, bool) {
	manager.mu.Lock()
	entry := manager.entries[id]
	manifest, exists := manager.manifests[id]
	manager.mu.Unlock()
	if entry == nil {
		if !exists {
			return nil, false
		}
		snapshot := processSnapshotFromManifest(manifest)
		return &snapshot, true
	}
	entry.mu.Lock()
	if processTerminal(entry.manifest.State) {
		entry.mu.Unlock()
		snapshot := manager.snapshotEntry(entry)
		return &snapshot, true
	}
	entry.manifest.StopReason = reason
	pid := entry.manifest.PID
	command := entry.command
	updated := cloneProcessManifest(entry.manifest)
	// A stop that loses the race to the exit writes nothing: the terminal manifest stands.
	_ = manager.recordLiveManifest(entry, updated)
	entry.mu.Unlock()
	if pid <= 0 || command == nil {
		manager.setLeftRunning(entry)
		snapshot := manager.snapshotEntry(entry)
		return &snapshot, true
	}
	manager.stopSignalled(entry)
	snapshot := manager.snapshotEntry(entry)
	return &snapshot, true
}

// stopSignalled sends SIGTERM to the process group, SIGKILL after the grace, and records the
// process as left running when even that did not end it. Stop uses it, and so does a start
// that finds a stop waiting for the process it just created.
func (manager *ManagedProcessManager) stopSignalled(entry *managedProcess) {
	manager.signalProcess(entry, false)
	if !waitForManagedProcess(entry, entry.killGrace) {
		manager.signalProcess(entry, true)
		if !waitForManagedProcess(entry, entry.killGrace) {
			manager.setLeftRunning(entry)
		}
	}
}

func (manager *ManagedProcessManager) Close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.mu.Unlock()
	manager.starting.Wait()
	manager.mu.Lock()
	entries := make([]*managedProcess, 0, len(manager.entries))
	for _, entry := range manager.entries {
		entries = append(entries, entry)
	}
	manager.mu.Unlock()
	// The entries are inspected outside manager.mu: a manifest write takes entry.mu first and
	// manager.mu second, so holding manager.mu while taking entry.mu here would deadlock.
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry.mu.Lock()
		running := entry.manifest.State == ProcessRunning
		id := entry.manifest.ID
		entry.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	// The stops run together: each one may wait kill_grace_ms for SIGTERM and again for
	// SIGKILL, so one after another held a close for two graces per process — about 16 s for
	// the 8 children max_running allows when they ignore SIGTERM, against one grace when they
	// are signalled at the same time (PH3 measured 7.9x). Every Stop still owns its own entry,
	// so nothing here is shared between them but this slice.
	failures := make([]error, len(ids))
	var stops sync.WaitGroup
	for i, id := range ids {
		stops.Add(1)
		go func() {
			defer stops.Done()
			if _, found := manager.Stop(id, "session_closed"); !found {
				failures[i] = fmt.Errorf("stop managed process %s: process not found", id)
			}
		}()
	}
	stops.Wait()
	// A stop returns as soon as the process is gone, while its finisher still writes the
	// last manifest and prunes older directories. Close waits for it, so a caller may
	// remove the storage folder once Close returned. It waits without a bound: the goroutine
	// holds no lock Close needs and its remaining work is local disk work, and it cannot
	// outlive the exit of its own process, which the Stop above waited for.
	manager.finishing.Wait()
	return errors.Join(failures...)
}

func (manager *ManagedProcessManager) Snapshot(id string) (*ProcessSnapshot, bool) {
	manager.mu.Lock()
	entry := manager.entries[id]
	manifest, exists := manager.manifests[id]
	manager.mu.Unlock()
	if entry != nil {
		snapshot := manager.snapshotEntry(entry)
		return &snapshot, true
	}
	if !exists {
		return nil, false
	}
	snapshot := processSnapshotFromManifest(manifest)
	return &snapshot, true
}

// failProcess finishes entry as failed with cause as the stop reason: the process never ran,
// or its wait did not come back, so there is no exit code or signal to report.
func (manager *ManagedProcessManager) failProcess(entry *managedProcess, cause string) {
	_ = manager.finish(entry, ProcessFailed, ProcessManifest{
		StopReason: cause,
		ExitCode:   json.RawMessage("null"),
		Signal:     json.RawMessage("null"),
	})
}

func (manager *ManagedProcessManager) waitForProcess(command *exec.Cmd, entry *managedProcess, id string) {
	defer manager.finishing.Done()
	// A panic in this detached goroutine (Wait, the collector, the manifest, prune) used to
	// take the server down and leave the process in the manifest as running: it is logged and
	// the process is finished as failed instead (P6BM-03).
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		slog.Error("process wait failed inside the server", "process", id, "panic", recovered)
		manager.failProcess(entry, "internal error")
	}()
	waitErr := command.Wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The command itself exited 0; only a descendant kept the pipes open.
		waitErr = nil
	}
	entry.mu.Lock()
	stdinWriter := entry.stdinWriter
	entry.mu.Unlock()
	if stdinWriter != nil {
		// Frees a writer still blocked on a process that never read its input.
		_ = stdinWriter.Close()
	}
	exitCode, hasExitCode, signal := bashExitStatus(waitErr)
	endProcessGroup(command, entry.killGrace)
	extra := ProcessManifest{Signal: json.RawMessage("null")}
	state := ProcessExited
	if hasExitCode {
		extra.ExitCode, _ = json.Marshal(exitCode)
	} else {
		extra.ExitCode = json.RawMessage("null")
	}
	if signal != "" {
		extra.Signal, _ = json.Marshal(signal)
	}
	if waitErr != nil && !hasExitCode && signal == "" {
		state = ProcessFailed
		extra.StopReason = waitErr.Error()
	}
	entry.mu.Lock()
	if entry.manifest.StopReason != "" {
		state = ProcessStopped
	}
	entry.mu.Unlock()
	_ = manager.finish(entry, state, extra)
}

// endProcessGroup runs once the command was reaped and Wait returned (output already
// drained, or cut at WaitDelay): a descendant still in the group is a leak, since the
// process tool is itself the background. It gets SIGTERM, then SIGKILL after grace. The
// group is polled, not timed, so a group id the system handed out again is never hit by
// the hard kill.
func endProcessGroup(command *exec.Cmd, grace time.Duration) {
	if command == nil || command.Process == nil {
		return
	}
	pgid := command.Process.Pid
	if !processGroupAlive(pgid) {
		return
	}
	signalProcessGroup(command, false)
	deadline := time.Now().Add(grace)
	for processGroupAlive(pgid) {
		if !time.Now().Before(deadline) {
			signalProcessGroup(command, true)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (manager *ManagedProcessManager) finish(entry *managedProcess, state ProcessState, extra ProcessManifest) error {
	entry.finishOnce.Do(func() {
		entry.mu.Lock()
		entry.collector.end()
		stdout := entry.stdout.close()
		stderr := entry.stderr.close()
		output := entry.collector.render()
		manifest := cloneProcessManifest(entry.manifest)
		manifest.State = state
		manifest.EndedAt = processNow()
		if extra.ExitCode != nil {
			manifest.ExitCode = append(json.RawMessage(nil), extra.ExitCode...)
		}
		if extra.Signal != nil {
			manifest.Signal = append(json.RawMessage(nil), extra.Signal...)
		}
		if extra.StopReason != "" {
			manifest.StopReason = extra.StopReason
		}
		manifest.StdoutProducedBytes = jsonRawInt64Value(output.stdout.producedBytes)
		manifest.StderrProducedBytes = jsonRawInt64Value(output.stderr.producedBytes)
		manifest.StdoutStoredBytes = jsonRawInt64Value(stdout.writtenBytes)
		manifest.StderrStoredBytes = jsonRawInt64Value(stderr.writtenBytes)
		manifest.StdoutCapped = jsonRawBoolValue(stdout.capped)
		manifest.StderrCapped = jsonRawBoolValue(stderr.capped)
		entry.manifest = manifest
		entry.mu.Unlock()

		manager.mu.Lock()
		manager.manifests[manifest.ID] = cloneProcessManifest(manifest)
		manager.mu.Unlock()
		if err := manager.persistManifest(manifest); err != nil {
			entry.finalizeErr = fmt.Errorf("persist process manifest: %w", err)
		}
		// The signal is queued before done closes, so a caller released by done that
		// marks the result read cannot race the completion.
		if entry.async != nil {
			entry.async.End(manager.snapshotEntry(entry))
		}
		// done closes before the prune: a Wait on this process must not also wait for
		// the removal of other, older processes' directories.
		close(entry.done)
		if err := manager.prune(entry.maxCompleted, entry.retentionDays); err != nil && entry.finalizeErr == nil {
			entry.finalizeErr = fmt.Errorf("prune managed processes: %w", err)
		}
	})
	return entry.finalizeErr
}

func (manager *ManagedProcessManager) snapshotEntry(entry *managedProcess) ProcessSnapshot {
	entry.mu.Lock()
	frozen := entry.collector.freeze()
	manifest := cloneProcessManifest(entry.manifest)
	stdoutStored := int64(0)
	stderrStored := int64(0)
	stdoutCapped := false
	stderrCapped := false
	if entry.stdout != nil {
		stdoutStored = entry.stdout.written
		stdoutCapped = entry.stdout.capped
	}
	if entry.stderr != nil {
		stderrStored = entry.stderr.written
		stderrCapped = entry.stderr.capped
	}
	entry.mu.Unlock()
	// Rendering walks both windows; the child's writer waits on entry.mu meanwhile
	// if it is still held.
	return ProcessSnapshot{
		ProcessManifest:   manifest,
		Output:            frozen.render(),
		StdoutStoredBytes: stdoutStored,
		StderrStoredBytes: stderrStored,
		StdoutCapped:      stdoutCapped,
		StderrCapped:      stderrCapped,
	}
}

func (manager *ManagedProcessManager) signalProcess(entry *managedProcess, force bool) {
	entry.mu.Lock()
	command := entry.command
	entry.mu.Unlock()
	signalProcessGroup(command, force)
}

func (manager *ManagedProcessManager) setLeftRunning(entry *managedProcess) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.manifest.LeftRunning = true
	_ = manager.recordLiveManifest(entry, cloneProcessManifest(entry.manifest))
}

// recordLiveManifest records manifest as the entry's manifest, in memory and on disk, but
// only while the entry is live. It must be called with entry.mu held: a stop that passed its
// own state check before the process exited would otherwise write "running" over the terminal
// manifest finish had already written, leaving a phantom process for the rest of the server's
// life (and after a restart). finish takes entry.mu before it sets the terminal state, so
// holding it here also orders the two writes.
func (manager *ManagedProcessManager) recordLiveManifest(entry *managedProcess, manifest ProcessManifest) error {
	if processTerminal(entry.manifest.State) {
		return nil
	}
	if hook := manager.liveHook; hook != nil {
		hook(manifest.ID)
	}
	manager.mu.Lock()
	manager.manifests[manifest.ID] = manifest
	manager.mu.Unlock()
	return manager.persistManifest(manifest)
}

// processDrainAfterExit is the configured tools.native.process.drain_after_exit_ms; a policy
// built without the setting (zero) keeps the bash default.
func processDrainAfterExit(ms int64) time.Duration {
	if ms <= 0 {
		return bashDrainAfterExit
	}
	return time.Duration(ms) * time.Millisecond
}

func waitForManagedProcess(entry *managedProcess, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-entry.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-entry.done:
		return true
	case <-timer.C:
		return false
	}
}

func processTerminal(state ProcessState) bool {
	return state != ProcessRunning
}

// buildNativeProcessEnvironment keeps only the allowlisted variables of base, the
// same set bash and exec pass, so a variable bash cannot read is not reachable
// through a managed process either. overrides (env_file, env) are added on top.
func buildNativeProcessEnvironment(base []string, overrides map[string]string, apiKeyEnv string, denied []string) []string {
	allowed := make(map[string]struct{}, len(bashEnvironmentAllowlist))
	for _, name := range bashEnvironmentAllowlist {
		allowed[name] = struct{}{}
	}
	values := make(map[string]string)
	for _, item := range base {
		name, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if _, ok := allowed[name]; ok {
			values[name] = value
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	delete(values, "JAD_API_TOKEN")
	if apiKeyEnv != "" {
		delete(values, apiKeyEnv)
	}
	for _, name := range denied {
		delete(values, name)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

func newManagedProcessID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(id[:])
	return "proc_" + encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func processNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func cloneProcessManifest(manifest ProcessManifest) ProcessManifest {
	manifest.ExitCode = append(json.RawMessage(nil), manifest.ExitCode...)
	manifest.Signal = append(json.RawMessage(nil), manifest.Signal...)
	manifest.StdoutProducedBytes = append(json.RawMessage(nil), manifest.StdoutProducedBytes...)
	manifest.StderrProducedBytes = append(json.RawMessage(nil), manifest.StderrProducedBytes...)
	manifest.StdoutStoredBytes = append(json.RawMessage(nil), manifest.StdoutStoredBytes...)
	manifest.StderrStoredBytes = append(json.RawMessage(nil), manifest.StderrStoredBytes...)
	manifest.StdoutCapped = append(json.RawMessage(nil), manifest.StdoutCapped...)
	manifest.StderrCapped = append(json.RawMessage(nil), manifest.StderrCapped...)
	return manifest
}

func processSnapshotFromManifest(manifest ProcessManifest) ProcessSnapshot {
	return ProcessSnapshot{
		ProcessManifest:   cloneProcessManifest(manifest),
		Output:            emptyBashRenderedOutput(),
		StdoutStoredBytes: rawJSONInt64(manifest.StdoutStoredBytes, 0),
		StderrStoredBytes: rawJSONInt64(manifest.StderrStoredBytes, 0),
		StdoutCapped:      rawJSONBool(manifest.StdoutCapped, false),
		StderrCapped:      rawJSONBool(manifest.StderrCapped, false),
	}
}

func emptyBashRenderedOutput() bashRenderedOutput {
	return bashRenderedOutput{
		stdout: bashStreamOutput{text: ""},
		stderr: bashStreamOutput{text: ""},
	}
}

func rawJSONInt64(raw json.RawMessage, fallback int64) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return fallback
	}
	return value
}

func jsonRawInt64Value(value int64) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func jsonRawBoolValue(value bool) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func rawJSONBool(raw json.RawMessage, fallback bool) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return fallback
	}
	return value
}

func (manager *ManagedProcessManager) Recover() error {
	manager.recoverMu.Lock()
	defer manager.recoverMu.Unlock()

	manager.mu.Lock()
	if manager.recovered {
		manager.mu.Unlock()
		return nil
	}
	if manager.closed {
		manager.mu.Unlock()
		return errProcessManagerClosed
	}
	manager.mu.Unlock()

	if err := os.MkdirAll(manager.directory, 0o755); err != nil {
		return fmt.Errorf("create process storage: %w", err)
	}
	entries, err := os.ReadDir(manager.directory)
	if err != nil {
		return fmt.Errorf("read process storage: %w", err)
	}
	manager.mu.Lock()
	stopOrphans, orphanGrace := manager.stopOrphans, manager.orphanGrace
	manager.mu.Unlock()
	var found []ProcessManifest
	var stoppable []int
	stoppableID := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, ok := manager.readManifest(entry.Name())
		if !ok || manifest.ID != entry.Name() {
			continue
		}
		if manifest.State == ProcessRunning {
			switch manager.classifyOrphan(manifest, stopOrphans) {
			case orphanElsewhere:
				// Another running server's process: not ours to mark lost, count,
				// list, prune or stop.
				continue
			case orphanStoppable:
				stoppable = append(stoppable, manifest.PID)
				stoppableID[manifest.ID] = true
			}
		}
		found = append(found, manifest)
	}
	gone := stopOrphanGroups(stoppable, orphanGrace)
	for _, manifest := range found {
		if manifest.State == ProcessRunning {
			manifest.State = ProcessLost
			manifest.EndedAt = processNow()
			manifest.endedBeforeStop = manifest.StopReason == ""
			manifest.StopReason = processReasonRestarted
			if stoppableID[manifest.ID] && gone[manifest.PID] {
				manifest.StopReason = processReasonRestartedStopped
			}
			if err := manager.persistManifest(manifest); err != nil {
				return fmt.Errorf("persist recovered process manifest %s: %w", manifest.ID, err)
			}
		}
		manager.mu.Lock()
		manager.manifests[manifest.ID] = cloneProcessManifest(manifest)
		manager.mu.Unlock()
	}
	// Retention used to run only on a finish, so a profile that stopped starting processes kept
	// every manifest and log it had (P6BM-24). max_completed is validated to at least 1, so a
	// manager built without settings prunes nothing here.
	manager.mu.Lock()
	maxCompleted, retentionDays := manager.maxCompleted, manager.retentionDays
	manager.mu.Unlock()
	if maxCompleted > 0 {
		if err := manager.prune(maxCompleted, retentionDays); err != nil {
			return fmt.Errorf("prune recovered processes: %w", err)
		}
	}
	manager.mu.Lock()
	manager.recovered = true
	manager.mu.Unlock()
	return nil
}

func (manager *ManagedProcessManager) persistManifest(manifest ProcessManifest) error {
	manager.persistMu.Lock()
	defer manager.persistMu.Unlock()

	directory := filepath.Join(manager.directory, manifest.ID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	encoded, err := marshalProcessManifest(manifest)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, "manifest-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, "manifest.json"))
}

func (manager *ManagedProcessManager) readManifest(id string) (ProcessManifest, bool) {
	data, err := os.ReadFile(filepath.Join(manager.directory, id, "manifest.json"))
	if err != nil {
		return ProcessManifest{}, false
	}
	var manifest ProcessManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ProcessManifest{}, false
	}
	if manifest.SchemaVersion != 1 || manifest.ID == "" || manifest.State == "" || manifest.StartedAt == "" {
		return ProcessManifest{}, false
	}
	manifest.endedBeforeStop = endedAtPrecedesStopReason(data)
	return manifest, true
}

func (manager *ManagedProcessManager) readWindows(id, name string, limits bashStreamLimits) ([][]byte, int64) {
	path := filepath.Join(manager.directory, id, name)
	file, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0
	}
	storedBytes := info.Size()
	if storedBytes <= 0 {
		return nil, 0
	}
	if limits.headBytes < 0 {
		limits.headBytes = 0
	}
	if limits.tailBytes < 0 {
		limits.tailBytes = 0
	}
	read := func(offset int64, length int) []byte {
		if length <= 0 {
			return nil
		}
		buffer := make([]byte, length)
		count, _ := file.ReadAt(buffer, offset)
		return buffer[:count]
	}
	windowBytes := int64(limits.headBytes) + int64(limits.tailBytes)
	if storedBytes <= windowBytes && storedBytes <= int64(int(storedBytes)) {
		whole := read(0, int(storedBytes))
		if len(whole) == 0 {
			return nil, 0
		}
		return [][]byte{whole}, storedBytes
	}
	headBytes := limits.headBytes
	if int64(headBytes) > storedBytes {
		headBytes = int(storedBytes)
	}
	tailBytes := limits.tailBytes
	if int64(tailBytes) > storedBytes-int64(headBytes) {
		tailBytes = int(storedBytes - int64(headBytes))
	}
	head := read(0, headBytes)
	tail := read(storedBytes-int64(tailBytes), tailBytes)
	chunks := make([][]byte, 0, 2)
	if len(head) > 0 {
		chunks = append(chunks, head)
	}
	if len(tail) > 0 {
		chunks = append(chunks, tail)
	}
	if len(chunks) == 0 {
		return nil, 0
	}
	return chunks, storedBytes
}

func (manager *ManagedProcessManager) removeEntry(id string) {
	manager.mu.Lock()
	delete(manager.entries, id)
	delete(manager.manifests, id)
	manager.mu.Unlock()
}

func (manager *ManagedProcessManager) prune(maxCompleted, retentionDays int64) error {
	manager.mu.Lock()
	completed := make([]ProcessManifest, 0, len(manager.manifests))
	for _, manifest := range manager.manifests {
		if processTerminal(manifest.State) {
			completed = append(completed, cloneProcessManifest(manifest))
		}
	}
	manager.mu.Unlock()
	sort.Slice(completed, func(i, j int) bool {
		left := completed[i].EndedAt
		if left == "" {
			left = completed[i].StartedAt
		}
		right := completed[j].EndedAt
		if right == "" {
			right = completed[j].StartedAt
		}
		return left > right
	})

	var cutoff time.Time
	if retentionDays > 0 {
		cutoff = time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	}
	var removable []ProcessManifest
	for index, manifest := range completed {
		remove := int64(index) >= maxCompleted
		if !remove && !cutoff.IsZero() {
			ended := manifest.EndedAt
			if ended == "" {
				ended = manifest.StartedAt
			}
			if parsed, err := time.Parse(time.RFC3339Nano, ended); err == nil && parsed.Before(cutoff) {
				remove = true
			}
		}
		if remove {
			removable = append(removable, manifest)
		}
	}
	if len(removable) == 0 {
		return nil
	}
	manager.mu.Lock()
	for _, manifest := range removable {
		current, exists := manager.manifests[manifest.ID]
		if !exists || !processTerminal(current.State) {
			continue
		}
		delete(manager.manifests, manifest.ID)
		delete(manager.entries, manifest.ID)
	}
	manager.mu.Unlock()
	var failures []error
	for _, manifest := range removable {
		if err := manager.removeAll(filepath.Join(manager.directory, manifest.ID)); err != nil {
			failures = append(failures, fmt.Errorf("remove managed process %s: %w", manifest.ID, err))
		}
	}
	return errors.Join(failures...)
}
