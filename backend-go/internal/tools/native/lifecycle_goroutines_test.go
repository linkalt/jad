package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func baselineCount() int {
	time.Sleep(200 * time.Millisecond)
	return runtime.NumGoroutine()
}

func settledCount(limit int) int {
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > limit && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

func requireNoGrowth(t *testing.T, before int, what string) {
	t.Helper()
	if after := settledCount(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after %s\n%s", before, after, what, buffer[:runtime.Stack(buffer, true)])
	}
}

// 50 foreground bash calls cancelled while the child is running must release every
// reader, waiter and timer goroutine of the call.
func TestCancelledBashCallsLeaveNoGoroutines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	run := func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		child := runBashCommand(ctx, bashChildRequest{
			file:        "/bin/sh",
			args:        []string{"-c", "sleep 30"},
			cwd:         t.TempDir(),
			environment: []string{"PATH=/usr/bin:/bin"},
			killGrace:   50 * time.Millisecond,
			collector:   newBashOutputCollector(bashStreamLimits{headBytes: 64, tailBytes: 64}, bashStreamLimits{headBytes: 64, tailBytes: 64}, 200, false),
		})
		if child.spawnError != nil {
			t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
		}
		if !child.aborted {
			t.Fatalf("call was not aborted: %+v", child)
		}
	}
	run()
	before := baselineCount()
	for index := 0; index < 50; index++ {
		run()
	}
	requireNoGrowth(t, before, "50 cancelled bash calls")
}

func TestCancelledRipgrepCallsLeaveNoGoroutines(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	root := t.TempDir()
	for index := 0; index < 400; index++ {
		path := filepath.Join(root, fmt.Sprintf("dir%d", index%20), fmt.Sprintf("f%d.txt", index))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Repeat("some ordinary line\n", 200)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := nativeGrepSettings(binary)
	run := func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Millisecond)
			cancel()
		}()
		_ = runRipgrep(ctx, settings, nil, "ordinary", []string{"."}, root)
		cancel()
	}
	run()
	before := baselineCount()
	for index := 0; index < 50; index++ {
		run()
	}
	requireNoGrowth(t, before, "50 cancelled ripgrep calls")
}

// Start, stop and wait for 30 managed processes: the waiter, the pipe writers and the
// timers of every process must be gone once it ended.
func TestManagedProcessStartStopCyclesLeaveNoGoroutines(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	cycle := func() {
		started := runner.run(map[string]any{"command": "sleep 30"})
		id := processIDOf(t, started)
		runner.run(map[string]any{"action": "stop", "id": id})
	}
	cycle()
	before := baselineCount()
	for index := 0; index < 30; index++ {
		cycle()
	}
	requireNoGrowth(t, before, "30 managed process start/stop cycles")
}

// The wait action runs manager.Wait on its own goroutine so a stopped run is not held by
// it; after the call is cancelled that goroutine lives until the wait timeout. This test
// bounds it: it must be gone by timeout_ms plus a margin, and the time is logged.
func TestCancelledProcessWaitGoroutineEndsAtItsTimeout(t *testing.T) {
	runner := newProcessStdinRunner(t, 1024)
	started := runner.run(map[string]any{"command": "sleep 30"})
	id := processIDOf(t, started)
	before := baselineCount()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	begun := time.Now()
	_, err := executeNativeProcess(ctx, map[string]any{"action": "wait", "id": id, "timeout_ms": json.Number("4000")}, tools.ExecutionContext{
		CallID: "call-wait", WorkspaceRoot: runner.workspace, SessionDir: runner.workspace,
	}, runner.settings, runner.manager, "")
	if err == nil {
		t.Fatal("the cancelled wait returned no error")
	}
	returned := time.Since(begun)
	afterCancel := runtime.NumGoroutine()
	requireNoGrowth(t, before, "a cancelled process wait")
	t.Logf("call returned after %v; goroutines right after return %d (before %d); all gone after %v", returned.Round(time.Millisecond), afterCancel, before, time.Since(begun).Round(time.Millisecond))
	runner.run(map[string]any{"action": "stop", "id": id})
}
