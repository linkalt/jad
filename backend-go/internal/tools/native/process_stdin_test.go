package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

type processStdinRunner struct {
	t         *testing.T
	manager   *ManagedProcessManager
	workspace string
	settings  sessionfs.NativeProcessSettings
	calls     int
}

func newProcessStdinRunner(t *testing.T, stdinMax int64) *processStdinRunner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	workspace := t.TempDir()
	manager := NewProcessManager(filepath.Join(workspace, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	settings := processTestRequest(workspace, "", 4096).Policy
	settings.Shell = "/bin/bash"
	settings.Stdout = sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512}
	settings.Stderr = sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512}
	settings.MaxLineChars = 2000
	settings.StdinMaxBytes = stdinMax
	return &processStdinRunner{t: t, manager: manager, workspace: workspace, settings: settings}
}

func (runner *processStdinRunner) run(arguments map[string]any) string {
	runner.t.Helper()
	runner.calls++
	output, err := executeNativeProcess(context.Background(), arguments, tools.ExecutionContext{
		CallID: "call-" + string(rune('a'+runner.calls)), WorkspaceRoot: runner.workspace, SessionDir: runner.workspace,
	}, runner.settings, runner.manager, "")
	if err != nil {
		runner.t.Fatalf("executeNativeProcess() error = %v", err)
	}
	return output
}

func goroutineCount() int { return runtime.NumGoroutine() }

var processIDPattern = regexp.MustCompile(`(?m)^process_id: (proc_\S+)$`)

func processIDOf(t *testing.T, output string) string {
	t.Helper()
	match := processIDPattern.FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("no process_id in %s", output)
	}
	return match[1]
}

func TestNativeProcessStartWritesStdinThenClosesIt(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	started := runner.run(map[string]any{"command": "cat", "stdin": "piped-text\nsecond line\n"})
	if !strings.Contains(started, `"status":"ok"`) {
		t.Fatalf("start = %s", started)
	}
	waited := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, started), "timeout_ms": json.Number("2000")})
	// `cat` only exits when its input is closed, so reaching `exited` proves EOF.
	if !strings.Contains(waited, "state: exited") || !strings.Contains(waited, "piped-text\nsecond line") {
		t.Fatalf("wait = %s", waited)
	}
	// Multi-byte text arrives intact and leading whitespace is kept.
	wide := runner.run(map[string]any{"command": "cat", "stdin": "  héllo ✓"})
	wideDone := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, wide), "timeout_ms": json.Number("2000")})
	if !strings.Contains(wideDone, "state: exited") || !strings.Contains(wideDone, "  héllo ✓") {
		t.Fatalf("multi-byte wait = %s", wideDone)
	}
}

func TestNativeProcessWithoutStdinSeesAClosedInput(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	started := runner.run(map[string]any{"command": "cat; echo no-input-done"})
	waited := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, started), "timeout_ms": json.Number("2000")})
	if !strings.Contains(waited, "state: exited") || !strings.Contains(waited, "no-input-done") {
		t.Fatalf("wait without stdin = %s", waited)
	}
	empty := runner.run(map[string]any{"command": "cat; echo empty-done", "stdin": ""})
	emptyDone := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, empty), "timeout_ms": json.Number("2000")})
	if !strings.Contains(emptyDone, "empty-done") {
		t.Fatalf("wait with empty stdin = %s", emptyDone)
	}
}

func TestNativeProcessThatNeverReadsItsStdinIsNotAnError(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024*1024)
	// 200 KiB into a process that exits without reading: the closed pipe must not fail the start.
	started := runner.run(map[string]any{"command": "echo ignored-input", "stdin": strings.Repeat("x", 200*1024)})
	if !strings.Contains(started, `"status":"ok"`) {
		t.Fatalf("start = %s", started)
	}
	waited := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, started), "timeout_ms": json.Number("2000")})
	if !strings.Contains(waited, "state: exited") || !strings.Contains(waited, "ignored-input") {
		t.Fatalf("wait = %s", waited)
	}
}

// The shell exits at once but a detached descendant keeps the read end of the pipe
// open and never reads it, so the write of a large text blocks. The writer must be
// released when the managed process ends; otherwise every such start leaks a goroutine.
func TestNativeProcessStdinWriterIsReleasedWhenTheProcessEnds(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024*1024)
	before := goroutineCount()
	// 512 KiB is far more than a pipe holds (64 KiB), so the write cannot complete.
	started := runner.run(map[string]any{"command": "setsid sleep 4 <&0 >/dev/null 2>&1 &", "stdin": strings.Repeat("y", 512*1024)})
	waited := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, started), "timeout_ms": json.Number("2000")})
	if !strings.Contains(waited, "state: exited") {
		t.Fatalf("the shell should have exited: %s", waited)
	}
	deadline := time.Now().Add(2 * time.Second)
	for goroutineCount() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := goroutineCount(); got > before {
		t.Fatalf("stdin writer goroutine leaked after the process ended: before %d, now %d", before, got)
	}
}

func TestNativeProcessStdinOverTheLimitIsRefusedBeforeAnythingStarts(t *testing.T) {
	runner := newProcessStdinRunner(t, 10)
	atLimit := runner.run(map[string]any{"command": "wc -c", "stdin": "0123456789"})
	if !strings.Contains(atLimit, `"status":"ok"`) {
		t.Fatalf("stdin at the limit = %s", atLimit)
	}
	marker := filepath.Join(runner.workspace, "should-not-start")
	over := runner.run(map[string]any{"command": "touch " + marker, "stdin": "01234567890"})
	if !strings.Contains(over, `"status":"invalid_input"`) || !strings.Contains(over, "error_code: stdin_too_large") ||
		!strings.Contains(over, "11 bytes, over the limit of 10") {
		t.Fatalf("stdin over the limit = %s", over)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused start still ran its command")
	}
	if got := len(runner.manager.List("")); got != 1 {
		t.Fatalf("processes after a refused start = %d, want 1", got)
	}
	// Three-byte characters: 4 of them are 12 bytes, not 4 characters.
	wide := runner.run(map[string]any{"command": "cat", "stdin": "✓✓✓✓"})
	if !strings.Contains(wide, "12 bytes, over the limit of 10") {
		t.Fatalf("multi-byte stdin over the limit = %s", wide)
	}
}

func TestNativeProcessStdinIsOnlyAcceptedOnStartAndIsNotStored(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	const secret = "SECRET-STDIN-TEXT"
	started := runner.run(map[string]any{"command": "cat", "stdin": secret})
	id := processIDOf(t, started)
	runner.run(map[string]any{"action": "wait", "id": id, "timeout_ms": json.Number("2000")})
	for _, arguments := range []map[string]any{
		{"action": "status", "id": id, "stdin": "x"},
		{"action": "wait", "id": id, "stdin": "x"},
		{"action": "stop", "id": id, "stdin": "x"},
		{"action": "list", "stdin": "x"},
	} {
		if refused := runner.run(arguments); !strings.Contains(refused, "error_code: invalid_stdin") {
			t.Errorf("%v = %s", arguments, refused)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(runner.workspace, "processes", id, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), secret) {
		t.Fatalf("the manifest stored the stdin text: %s", manifest)
	}
}

func TestNormalizeProcessArgumentsAcceptsInputAsStdin(t *testing.T) {
	normalized := normalizeProcessArguments(map[string]any{"command": "cat", "input": "via-input"})
	if normalized["stdin"] != "via-input" {
		t.Fatalf("input was not mapped to stdin: %#v", normalized)
	}
	if _, exists := normalized["input"]; exists {
		t.Fatalf("normalized arguments retained input: %#v", normalized)
	}
	both := normalizeProcessArguments(map[string]any{"command": "cat", "stdin": "named", "input": "alias"})
	if both["stdin"] != "named" {
		t.Fatalf("the named field did not win: %#v", both)
	}
}

// A process started through PATH is launched from an absolute path, but a script
// that prints $0 has to see the shell name as it was configured, as it does under
// Node. Same rule as for bash; see TestNativeBashKeepsTheConfiguredNameAsArgvZero.
func TestNativeProcessKeepsTheConfiguredShellNameAsArgvZero(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	binaryDir := filepath.Join(runner.workspace, "bin")
	if err := os.Mkdir(binaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(runner.settings.Shell, filepath.Join(binaryDir, "argv0-shell")); err != nil {
		t.Fatal(err)
	}
	runner.settings.Shell = "argv0-shell"
	runner.settings.Env = map[string]string{"PATH": binaryDir}
	started := runner.run(map[string]any{"command": `printf 'name=%s\n' "$0"`})
	waited := runner.run(map[string]any{"action": "wait", "id": processIDOf(t, started), "timeout_ms": json.Number("2000")})
	if !strings.Contains(waited, "name=argv0-shell\n") || strings.Contains(waited, binaryDir) {
		t.Fatalf("the process saw its resolved path instead of the configured name: %s", waited)
	}
}

func TestNativeProcessWaitReturnsWhenTheRunIsStopped(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	started := runner.run(map[string]any{"command": "sleep 30"})
	id := processIDOf(t, started)
	runner.settings.MaxWaitTimeoutMs = 60000
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executeNativeProcess(ctx, map[string]any{"action": "wait", "id": id, "timeout_ms": json.Number("60000")}, tools.ExecutionContext{
			CallID: "call-wait", WorkspaceRoot: runner.workspace, SessionDir: runner.workspace,
		}, runner.settings, runner.manager, "")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait kept blocking after the run was stopped")
	}
}
