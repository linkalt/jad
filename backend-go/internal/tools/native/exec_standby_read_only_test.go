package native

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// standbyStarts records the sandbox of every standby the pool tries to start; none starts.
func standbyStarts(t *testing.T) func(time.Duration) []string {
	t.Helper()
	pool := isolateStandbyPool(t)
	var mu sync.Mutex
	var started []string
	pool.start = func(spawn standbySpawn) (*standbyProcess, error) {
		mu.Lock()
		defer mu.Unlock()
		paths := "no sandbox"
		if spawn.sandbox != nil {
			paths = strings.Join(spawn.sandbox.WritablePaths, ",")
		}
		started = append(started, paths)
		return nil, errors.New("not started in the test")
	}
	// wait returns what started once no further start came for quiet.
	return func(quiet time.Duration) []string {
		previous := -1
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(quiet) {
			mu.Lock()
			count := len(started)
			mu.Unlock()
			if count == previous && count > 0 {
				break
			}
			previous = count
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), started...)
	}
}

func takeStandbyFor(t *testing.T, settings sessionfs.NativeExecSettings, root string, planMode bool) {
	t.Helper()
	execution := tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: root, PlanMode: planMode}
	plan, problem := planExec(map[string]any{"lang": "python", "code": "1"}, settings, execution)
	if problem != "" {
		t.Fatal(problem)
	}
	if process := takeExecStandby(plan, settings, "", execution, ".", nil, nil, nil); process != nil {
		t.Fatal("the pool held a standby before any start")
	}
}

// Under a bwrap or seatbelt sandbox a call that may write warms its own standby and then the
// plan-mode form a read_only call starts in; a plan-mode call, or a call with no sandbox,
// warms only its own.
func TestExecStandbyWarmsTheReadOnlyFormUnderASandbox(t *testing.T) {
	sandboxed := func(t *testing.T, root string) sessionfs.NativeExecSettings {
		settings := standbyExecSettings()
		settings.Child = bashTestSettings(t, root)
		settings.Child.Sandbox = bwrapSandbox()
		settings.Child.Sandbox.PlanWritablePaths = []string{"{tmp}"}
		return settings
	}
	t.Run("call that may write", func(t *testing.T) {
		wait := standbyStarts(t)
		root := t.TempDir()
		takeStandbyFor(t, sandboxed(t, root), root, false)
		got := wait(50 * time.Millisecond)
		if len(got) != 2 || !containsString(got, "{tmp}") || !containsString(got, "{workspace},{git_common_dir},{tmp}") {
			t.Fatalf("started %v, want the call's own form and the read_only one", got)
		}
	})
	t.Run("plan-mode call", func(t *testing.T) {
		wait := standbyStarts(t)
		root := t.TempDir()
		takeStandbyFor(t, sandboxed(t, root), root, true)
		if got := wait(50 * time.Millisecond); len(got) != 1 || got[0] != "{tmp}" {
			t.Fatalf("started %v, want only the plan-mode form", got)
		}
	})
	t.Run("no sandbox", func(t *testing.T) {
		wait := standbyStarts(t)
		root := t.TempDir()
		settings := standbyExecSettings()
		settings.Child = bashTestSettings(t, root)
		takeStandbyFor(t, settings, root, false)
		if got := wait(50 * time.Millisecond); len(got) != 1 || got[0] != "no sandbox" {
			t.Fatalf("started %v, want one standby", got)
		}
	})
}
