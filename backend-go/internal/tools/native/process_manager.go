package native

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
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
}

type ProcessWaitResult struct {
	Snapshot *ProcessSnapshot
	Found    bool
	Waited   bool
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
	persistMu sync.Mutex
	recoverMu sync.Mutex
	recovered bool
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
}

type processStreamWriter struct {
	entry  *managedProcess
	stream bashStream
}

func NewProcessManager(directory string) *ManagedProcessManager {
	return &ManagedProcessManager{
		directory: filepath.Clean(directory),
		entries:   make(map[string]*managedProcess),
		manifests: make(map[string]ProcessManifest),
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

func (manager *ManagedProcessManager) Start(request ProcessStartRequest) (ProcessSnapshot, error) {
	if runtime.GOOS == "windows" {
		return ProcessSnapshot{}, errors.New("managed process groups require a POSIX platform")
	}
	if request.Command == "" {
		return ProcessSnapshot{}, errors.New("process command is empty")
	}
	if err := manager.Recover(); err != nil {
		return ProcessSnapshot{}, err
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return ProcessSnapshot{}, errors.New("process manager is closed")
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
		return ProcessSnapshot{}, fmt.Errorf("create process storage: %w", err)
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
	}
	manager.entries[id] = entry
	manager.manifests[id] = manifest
	manager.starting.Add(1)
	manager.mu.Unlock()
	defer manager.starting.Done()

	if err := manager.persistManifest(manifest); err != nil {
		manager.removeEntry(id)
		_ = os.RemoveAll(processDir)
		return ProcessSnapshot{}, fmt.Errorf("persist process manifest: %w", err)
	}
	environment := buildNativeProcessEnvironment(request.Environment, request.Policy.Env, request.APIKeyEnv, request.Policy.EnvDeny)
	resolvedShell, err := resolveBashExecutable(request.Policy.Shell, environment, request.CWD)
	if err != nil {
		_ = manager.finish(entry, ProcessFailed, ProcessManifest{
			StopReason: err.Error(),
			ExitCode:   json.RawMessage("null"),
			Signal:     json.RawMessage("null"),
		})
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
			_ = manager.finish(entry, ProcessFailed, ProcessManifest{
				StopReason: pipeErr.Error(),
				ExitCode:   json.RawMessage("null"),
				Signal:     json.RawMessage("null"),
			})
			return manager.snapshotEntry(entry), nil
		}
		command.Stdin = reader
		stdinReader, stdinWriter = reader, writer
	}
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		if stdinReader != nil {
			_ = stdinReader.Close()
			_ = stdinWriter.Close()
		}
		_ = manager.finish(entry, ProcessFailed, ProcessManifest{
			StopReason: err.Error(),
			ExitCode:   json.RawMessage("null"),
			Signal:     json.RawMessage("null"),
		})
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
	manifest = cloneProcessManifest(entry.manifest)
	entry.mu.Unlock()
	manager.mu.Lock()
	manager.manifests[id] = manifest
	manager.mu.Unlock()
	go manager.waitForProcess(command, entry)
	if err := manager.persistManifest(manifest); err != nil {
		manager.signalProcess(entry, true)
		_ = waitForManagedProcess(entry, 2*time.Second)
		return ProcessSnapshot{}, fmt.Errorf("persist running process manifest: %w", err)
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
	entry.mu.Unlock()
	manager.mu.Lock()
	manager.manifests[id] = updated
	manager.mu.Unlock()
	_ = manager.persistManifest(updated)
	if pid <= 0 || command == nil {
		manager.setLeftRunning(entry)
		snapshot := manager.snapshotEntry(entry)
		return &snapshot, true
	}
	manager.signalProcess(entry, false)
	if !waitForManagedProcess(entry, entry.killGrace) {
		manager.signalProcess(entry, true)
		if !waitForManagedProcess(entry, entry.killGrace) {
			manager.setLeftRunning(entry)
		}
	}
	snapshot := manager.snapshotEntry(entry)
	return &snapshot, true
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
	ids := make([]string, 0, len(manager.entries))
	for id, entry := range manager.entries {
		entry.mu.Lock()
		running := entry.manifest.State == ProcessRunning
		entry.mu.Unlock()
		if running {
			ids = append(ids, id)
		}
	}
	manager.mu.Unlock()
	var failures []error
	for _, id := range ids {
		if _, found := manager.Stop(id, "session_closed"); !found {
			failures = append(failures, fmt.Errorf("stop managed process %s: process not found", id))
		}
	}
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

func (manager *ManagedProcessManager) waitForProcess(command *exec.Cmd, entry *managedProcess) {
	waitErr := command.Wait()
	entry.mu.Lock()
	stdinWriter := entry.stdinWriter
	entry.mu.Unlock()
	if stdinWriter != nil {
		// Frees a writer still blocked on a process that never read its input.
		_ = stdinWriter.Close()
	}
	exitCode, hasExitCode, signal := bashExitStatus(waitErr)
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
		if err := manager.prune(entry.maxCompleted, entry.retentionDays); err != nil && entry.finalizeErr == nil {
			entry.finalizeErr = fmt.Errorf("prune managed processes: %w", err)
		}
		close(entry.done)
	})
	return entry.finalizeErr
}

func (manager *ManagedProcessManager) snapshotEntry(entry *managedProcess) ProcessSnapshot {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	output := entry.collector.render()
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
	return ProcessSnapshot{
		ProcessManifest:   manifest,
		Output:            output,
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
	entry.manifest.LeftRunning = true
	manifest := cloneProcessManifest(entry.manifest)
	entry.mu.Unlock()
	manager.mu.Lock()
	manager.manifests[manifest.ID] = manifest
	manager.mu.Unlock()
	_ = manager.persistManifest(manifest)
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

func buildNativeProcessEnvironment(base []string, overrides map[string]string, apiKeyEnv string, denied []string) []string {
	values := make(map[string]string)
	for _, item := range base {
		name, value, found := strings.Cut(item, "=")
		if found {
			values[name] = value
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	delete(values, "AGENT_API_TOKEN")
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
		return errors.New("process manager is closed")
	}
	manager.mu.Unlock()

	if err := os.MkdirAll(manager.directory, 0o755); err != nil {
		return fmt.Errorf("create process storage: %w", err)
	}
	entries, err := os.ReadDir(manager.directory)
	if err != nil {
		return fmt.Errorf("read process storage: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, ok := manager.readManifest(entry.Name())
		if !ok || manifest.ID != entry.Name() {
			continue
		}
		if manifest.State == ProcessRunning {
			manifest.State = ProcessLost
			manifest.EndedAt = processNow()
			manifest.endedBeforeStop = manifest.StopReason == ""
			manifest.StopReason = "backend_restarted"
			if err := manager.persistManifest(manifest); err != nil {
				return fmt.Errorf("persist recovered process manifest %s: %w", manifest.ID, err)
			}
		}
		manager.mu.Lock()
		manager.manifests[manifest.ID] = cloneProcessManifest(manifest)
		manager.mu.Unlock()
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
		if err := os.RemoveAll(filepath.Join(manager.directory, manifest.ID)); err != nil {
			failures = append(failures, fmt.Errorf("remove managed process %s: %w", manifest.ID, err))
		}
	}
	return errors.Join(failures...)
}
