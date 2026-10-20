//go:build !windows

package native

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func startLeftover(t *testing.T, ctx context.Context, root string, sweep bool) int {
	t.Helper()
	settings := bashTestSettings(t, root)
	settings.KillProcessGroup = "timeout_only"
	settings.KillChildrenOnRunEnd = sweep
	output := executeNativeBash(ctx, map[string]any{"command": "sleep 300 >/dev/null 2>&1 & echo pid=$!"}, settings, "", tools.ExecutionContext{
		CallID: "leftover", WorkspaceRoot: root, SessionDir: root,
	})
	match := regexp.MustCompile("pid=([0-9]+)").FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("no pid in %s", output)
	}
	pid, _ := strconv.Atoi(match[1])
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func alive(pid int) bool {
	// A killed child stays a zombie until its parent reaps it; signal 0 still succeeds then.
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	text := string(data)
	return !strings.HasPrefix(text[strings.LastIndex(text, ")")+2:], "Z")
}

func waitDead(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRunEndSweepStopsLeftoverButKeepsItBetweenCalls(t *testing.T) {
	root := t.TempDir()
	ctx, sweep := WithChildGroups(context.Background())
	pid := startLeftover(t, ctx, root, true)
	second := executeNativeBash(ctx, map[string]any{"command": "true"}, bashTestSettings(t, root), "", tools.ExecutionContext{CallID: "second", WorkspaceRoot: root, SessionDir: root})
	if !alive(pid) {
		t.Fatalf("leftover died before the run ended; second call: %s", second)
	}
	sweep()
	if !waitDead(pid) {
		t.Fatal("leftover survived the run end")
	}
}

func TestRunEndSweepOffLeavesTheChild(t *testing.T) {
	ctx, sweep := WithChildGroups(context.Background())
	pid := startLeftover(t, ctx, t.TempDir(), false)
	sweep()
	time.Sleep(300 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("kill_children_on_run_end off still stopped the child")
	}
}

func TestRunEndSweepSkipsProcessToolProcesses(t *testing.T) {
	root := t.TempDir()
	_, sweep := WithChildGroups(context.Background())
	manager := NewProcessManager(filepath.Join(root, "processes"))
	t.Cleanup(func() { _ = manager.Close() })
	snapshot, err := manager.Start(processTestRequest(root, "sleep 300", 1024))
	if err != nil {
		t.Fatal(err)
	}
	pid := snapshot.PID
	sweep()
	time.Sleep(300 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("run end stopped a process-tool process")
	}
}

func TestRunEndSweepAfterCancelledRun(t *testing.T) {
	root := t.TempDir()
	base, cancel := context.WithCancel(context.Background())
	ctx, sweep := WithChildGroups(base)
	pid := startLeftover(t, ctx, root, true)
	cancel()
	sweep()
	if !waitDead(pid) {
		t.Fatal("leftover survived a cancelled run")
	}
}

func leftoverPID(t *testing.T, output string) int {
	t.Helper()
	match := regexp.MustCompile("pid=([0-9]+)").FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("no pid in %s", output)
	}
	pid, _ := strconv.Atoi(match[1])
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func startExecLeftover(t *testing.T, ctx context.Context, root string, sweep bool) int {
	t.Helper()
	bash := bashTestSettings(t, root)
	bash.KillProcessGroup = "timeout_only"
	bash.KillChildrenOnRunEnd = sweep
	output := runExecWith(ctx, root, execTestSettings(), bash, map[string]any{"lang": "bash", "code": "sleep 300 >/dev/null 2>&1 & echo pid=$!"})
	return leftoverPID(t, output)
}

func startToolSmithLeftover(t *testing.T, ctx context.Context, root string, sweep bool) int {
	t.Helper()
	settings := toolSmithTestSettings(t, root)
	settings.Child.KillProcessGroup = "timeout_only"
	settings.Child.KillChildrenOnRunEnd = sweep
	code := "const cp = await import('node:child_process'); const c = cp.spawn('sleep', ['300'], {stdio: 'ignore'}); c.unref(); return 'pid=' + c.pid;"
	return leftoverPID(t, runToolSmith(ctx, root, settings, smithRun(code, nil)))
}

func TestRunEndSweepCoversExecAndToolSmith(t *testing.T) {
	starters := map[string]func(*testing.T, context.Context, string, bool) int{
		"exec": startExecLeftover, "tool_smith": startToolSmithLeftover,
	}
	for name, start := range starters {
		t.Run(name, func(t *testing.T) {
			ctx, sweep := WithChildGroups(context.Background())
			pid := start(t, ctx, t.TempDir(), true)
			if !alive(pid) {
				t.Fatal("leftover died before the run ended")
			}
			sweep()
			if !waitDead(pid) {
				t.Fatal("leftover survived the run end")
			}
		})
		t.Run(name+" off", func(t *testing.T) {
			ctx, sweep := WithChildGroups(context.Background())
			pid := start(t, ctx, t.TempDir(), false)
			sweep()
			time.Sleep(300 * time.Millisecond)
			if !alive(pid) {
				t.Fatal("kill_children_on_run_end off still stopped the child")
			}
		})
	}
}

func runExecWith(ctx context.Context, root string, settings sessionfs.NativeExecSettings, bash sessionfs.NativeBashSettings, arguments map[string]any) string {
	settings.Child = bash
	return executeNativeExec(ctx, normalizeExecArguments(arguments, settings.DefaultLang), settings, "", tools.ExecutionContext{
		CallID: "exec-call", WorkspaceRoot: root, SessionDir: root,
	})
}

// Mirrors the spawn in .jad/functions/exec.ts background mode (shell, ignored stdio, unref, not detached); that file is gitignored.
func TestRunEndSweepCoversToolSmithBackgroundJob(t *testing.T) {
	spawn := func(detached bool) string {
		return "const cp = await import('node:child_process'); const c = cp.spawn('sleep 300', [], {shell: true, detached: " + strconv.FormatBool(detached) + ", stdio: ['ignore', 'ignore', 'ignore']}); c.unref(); return 'pid=' + c.pid;"
	}
	start := func(t *testing.T, ctx context.Context, detached, sweep bool) int {
		root := t.TempDir()
		settings := toolSmithTestSettings(t, root)
		settings.Child.KillProcessGroup = "timeout_only"
		settings.Child.KillChildrenOnRunEnd = sweep
		return leftoverPID(t, runToolSmith(ctx, root, settings, smithRun(spawn(detached), nil)))
	}
	t.Run("survives the call, dead after the sweep", func(t *testing.T) {
		ctx, sweep := WithChildGroups(context.Background())
		pid := start(t, ctx, false, true)
		if !alive(pid) {
			t.Fatal("background job died with the call")
		}
		sweep()
		if !waitDead(pid) {
			t.Fatal("background job survived the run end")
		}
	})
	t.Run("off keeps it", func(t *testing.T) {
		ctx, sweep := WithChildGroups(context.Background())
		pid := start(t, ctx, false, false)
		sweep()
		time.Sleep(300 * time.Millisecond)
		if !alive(pid) {
			t.Fatal("kill_children_on_run_end off still stopped the job")
		}
	})
	t.Run("detached control escapes the sweep", func(t *testing.T) {
		ctx, sweep := WithChildGroups(context.Background())
		pid := start(t, ctx, true, true)
		sweep()
		time.Sleep(300 * time.Millisecond)
		if !alive(pid) {
			t.Fatal("detached job was swept; the control no longer shows why detached was removed")
		}
	})
}
