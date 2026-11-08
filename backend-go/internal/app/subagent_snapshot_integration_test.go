package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// subagentSnapshotOf finds the subagent snapshot on the tool_result parts of a served
// session snapshot, and every tool_result part seen (to check for stray snapshots).
func subagentSnapshotOf(t *testing.T, body []byte) (snapshots []map[string]any, toolResults int) {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, body)
	}
	for _, message := range decoded.Messages {
		for _, part := range message.Parts {
			if part["type"] != "tool_result" {
				continue
			}
			toolResults++
			if snapshot, ok := part["subagent"].(map[string]any); ok {
				snapshots = append(snapshots, snapshot)
			}
		}
	}
	return snapshots, toolResults
}

// sessionBody is the session snapshot the browser reloads.
func (f subagentFixture) sessionSnapshot(t *testing.T) []byte {
	t.Helper()
	status, body := f.get(t, "")
	if status != http.StatusOK {
		t.Fatalf("GET session status = %d, body=%s", status, body)
	}
	return []byte(body)
}

// TestSubagentToolResultCarriesTheChildsCompletedStepsLiveAndInTheServedSession is the
// whole path: a real run starts a child, the live stream's tool_result part carries the
// child's steps, and the session snapshot the browser reloads carries the same ones.
func TestSubagentToolResultCarriesTheChildsCompletedStepsLiveAndInTheServedSession(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "instructions": "You are the explorer.", "description": "Finds facts."})
	live := f.run(t)
	if !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	// Live: a ui.part (or message) event with the tool_result part names the snapshot.
	if !strings.Contains(live, `"subagent":{"agent":"explore","steps":[{"id":"1.1","status":"completed"`) {
		t.Fatalf("the live stream carries no subagent snapshot:\n%s", live)
	}

	served := f.sessionSnapshot(t)
	snapshots, results := subagentSnapshotOf(t, served)
	if results != 1 || len(snapshots) != 1 {
		t.Fatalf("served tool results = %d, with a snapshot = %d, want 1 and 1:\n%s", results, len(snapshots), served)
	}
	snapshot := snapshots[0]
	if snapshot["agent"] != "explore" {
		t.Fatalf("snapshot agent = %v", snapshot["agent"])
	}
	steps, _ := snapshot["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("snapshot steps = %d, want the child's 2 rounds: %v", len(steps), snapshot)
	}
	for _, raw := range steps {
		step := raw.(map[string]any)
		if step["status"] != "completed" {
			t.Fatalf("step status = %v, want completed", step["status"])
		}
		for _, rawPart := range step["parts"].([]any) {
			switch kind := rawPart.(map[string]any)["type"]; kind {
			case "text", "reasoning", "tool_call":
			default:
				t.Fatalf("snapshot part type %v is not text, reasoning or tool_call", kind)
			}
		}
	}
	if !strings.Contains(string(served), "the answer is 42") {
		t.Fatalf("the child's answer is missing from the served snapshot")
	}

	// A message_page style read (a snapshot page for older turns) and the one-message route
	// go through the same attach, so a refetch of the history shows the same snapshot.
	if again := f.sessionSnapshot(t); string(again) != string(served) {
		t.Fatalf("a second read of the same session differs:\n%s\n%s", served, again)
	}
}
