package native

import "testing"

// TestCallRunningAsksOnlyTheRunsTheManagerHolds: a nil manager, an empty call id and a call
// the manager never ran are not running; a run counts only while it is queued or running,
// and only for the session that started it.
func TestCallRunningAsksOnlyTheRunsTheManagerHolds(t *testing.T) {
	var none *SubagentManager
	if none.CallRunning("s", "c") {
		t.Fatal("a nil manager reports a run")
	}
	manager := NewSubagentManager()
	defer manager.Close()
	manager.runs["sub_1"] = &subagentRun{id: "sub_1", session: "s", callID: "c", state: subagentRunRunning}
	manager.runs["sub_2"] = &subagentRun{id: "sub_2", session: "s", callID: "queued", state: subagentRunQueued}
	manager.runs["sub_3"] = &subagentRun{id: "sub_3", session: "s", callID: "ended", state: subagentRunDone}
	manager.runs["sub_4"] = &subagentRun{id: "sub_4", session: "s", callID: "lost", state: subagentRunLost}
	for call, want := range map[string]bool{"c": true, "queued": true, "ended": false, "lost": false, "unknown": false, "": false} {
		if got := manager.CallRunning("s", call); got != want {
			t.Errorf("CallRunning(s, %q) = %v, want %v", call, got, want)
		}
	}
	if manager.CallRunning("other", "c") {
		t.Error("another session's run is reported for this call id")
	}
}
