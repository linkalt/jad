package native

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// A turn that ends with two running processes and a running sub-agent leaves all three
// alive; in the next turn a stop (which reports the run itself) and one wait that returns
// both finished processes together.
func TestTurnEndsWithProcessAndSubagentRunningAndTheNextWaitCollectsBoth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	before := baselineCount()
	{
		m := newMixedHandles(t)
		defer m.close()
		turn, endTurn := context.WithCancel(context.Background())
		exec := tools.ExecutionContext{CallID: "c", WorkspaceRoot: m.dir, SessionKey: "sess|a"}
		out, err := m.process.Execute(turn, map[string]any{"command": "sleep 0.4; exit 3", "block_ms": json.Number("20")}, exec)
		if err != nil {
			t.Fatal(err)
		}
		processID := processIDOf(t, out)
		out, err = m.sub.Execute(turn, map[string]any{"task": "research", "block_ms": json.Number("20")}, exec)
		if err != nil {
			t.Fatal(err)
		}
		runID := runIDOf(t, out)
		out, err = m.process.Execute(turn, map[string]any{"command": "sleep 0.4", "block_ms": json.Number("20")}, exec)
		if err != nil {
			t.Fatal(err)
		}
		secondID := processIDOf(t, out)
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

		got := callAsync(t, m.async, "sess|a", map[string]any{"action": "wait", "block_ms": json.Number("5000")})
		if !strings.Contains(got, processID) || !strings.Contains(got, secondID) || strings.Contains(got, runID) {
			t.Fatalf("one wait did not return both completions:\n%s", got)
		}
		entry, err := m.host.Manager().Status("sess|a", processID)
		if err != nil || entry.OK || entry.Code != "3" {
			t.Fatalf("the process completion = %+v, %v; want ok false and exit code 3", entry, err)
		}
		again := callAsync(t, m.async, "sess|a", map[string]any{"action": "wait", "block_ms": json.Number("0")})
		if strings.Contains(again, processID) || strings.Contains(again, secondID) {
			t.Fatalf("a collected signal was delivered twice:\n%s", again)
		}
		if c := m.host.Manager().Counts("sess|a"); c.Active != 0 || c.Unread != 0 {
			t.Fatalf("counts after collecting = %+v", c)
		}
	}
	requireGoroutinesSettle(t, before)
}
