package native

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// mixedHandles is an exec tool and a sub-agent tool on one async host.
type mixedHandles struct {
	host  *AsyncHost
	exec  tools.Tool
	sub   tools.Tool
	async tools.Tool
	dir   string
	close func()
}

func newMixedHandles(t *testing.T) mixedHandles {
	t.Helper()
	requireInterpreters(t, "bash")
	dir := t.TempDir()
	host, err := NewAsyncHost(filepath.Join(dir, ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, dir)
	settings.Child.TimeoutMs, settings.Child.MaxTimeoutMs = 20_000, 30_000
	execTool, err := NewExecTool(settings, "KEY", host)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewPersistentSubagentManager(SubagentManagerOptions{StorageDir: filepath.Join(dir, "sub"), Keep: 10})
	launcher := &fakeLauncher{run: func(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
		<-ctx.Done()
		return SubagentResult{Text: "partial"}, ctx.Err()
	}}
	sub, err := newSubagentToolAsync(subagentSettings(agentNamed("explore")), withExecutors(t, readOnlyTool("read")), nil, launcher, nil, manager, host)
	if err != nil {
		t.Fatal(err)
	}
	async, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	return mixedHandles{host: host, exec: execTool, sub: sub, async: async, dir: dir, close: func() {
		_ = manager.Close()
		host.Close()
	}}
}

// startExec starts code in bash and returns the id of its handle.
func (m mixedHandles) startExec(ctx context.Context, t *testing.T, execution tools.ExecutionContext, code string, blockMs int) string {
	t.Helper()
	out, err := m.exec.Execute(ctx, map[string]any{"lang": "bash", "code": code, "block_ms": json.Number(fmt.Sprint(blockMs))}, execution)
	if err != nil {
		t.Fatal(err)
	}
	return execRunIDOf(t, out)
}

func waitForState(t *testing.T, host *AsyncHost, owner, id string, want asyncmgr.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, err := host.Manager().Status(owner, id); err == nil && entry.State == want {
			return
		}
		if time.Now().After(deadline) {
			entry, err := host.Manager().Status(owner, id)
			t.Fatalf("%s = %+v, %v; want %s", id, entry, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A turn that ends with two running exec handles and a running sub-agent leaves all three
// alive; in the next turn a stop (which reports the run itself) and one wait that returns
// both finished processes together.
func TestTurnEndsWithExecAndSubagentRunningAndTheNextWaitCollectsBoth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec process groups require a POSIX platform")
	}
	before := baselineCount()
	{
		m := newMixedHandles(t)
		defer m.close()
		turn, endTurn := context.WithCancel(context.Background())
		exec := tools.ExecutionContext{CallID: "c", WorkspaceRoot: m.dir, SessionDir: m.dir, SessionKey: "sess|a"}
		processID := m.startExec(turn, t, exec, "sleep 0.4; exit 3", 20)
		out, err := m.sub.Execute(turn, map[string]any{"task": "research", "block_ms": json.Number("20")}, exec)
		if err != nil {
			t.Fatal(err)
		}
		runID := runIDOf(t, out)
		secondID := m.startExec(turn, t, exec, "sleep 0.4", 20)
		endTurn()

		counts := m.host.Manager().Counts("sess|a")
		if counts.Active != 3 {
			t.Fatalf("after the turn ended: %+v, want both handles alive", counts)
		}
		if got := callAsync(t, m.async, "sess|b", map[string]any{"action": "list"}); strings.Contains(got, processID) || strings.Contains(got, runID) {
			t.Fatalf("another session lists these handles:\n%s", got)
		}

		if got := callAsync(t, m.async, "sess|a", map[string]any{"action": "stop", "id": runID}); !strings.Contains(got, runID) {
			t.Fatalf("stop did not report the run:\n%s", got)
		}
		waitForState(t, m.host, "sess|a", processID, asyncmgr.StateDone)
		waitForState(t, m.host, "sess|a", secondID, asyncmgr.StateDone)

		got := callAsync(t, m.async, "sess|a", map[string]any{"action": "wait", "wait_ms": json.Number("5000")})
		if !strings.Contains(got, processID) || !strings.Contains(got, secondID) || strings.Contains(got, runID) {
			t.Fatalf("one wait did not return both completions:\n%s", got)
		}
		entry, err := m.host.Manager().Status("sess|a", processID)
		if err != nil || entry.OK || entry.Code != "3" {
			t.Fatalf("the process completion = %+v, %v; want ok false and exit code 3", entry, err)
		}
		again := callAsync(t, m.async, "sess|a", map[string]any{"action": "wait", "wait_ms": json.Number("0")})
		if strings.Contains(again, processID) || strings.Contains(again, secondID) {
			t.Fatalf("a collected signal was delivered twice:\n%s", again)
		}
		if c := m.host.Manager().Counts("sess|a"); c.Active != 0 || c.Unread != 0 {
			t.Fatalf("counts after collecting = %+v", c)
		}
	}
	requireGoroutinesSettle(t, before)
}
