package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// subagentCallIDs lists the call ids of the subagent tool calls in a served session snapshot.
func subagentCallIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Parts []map[string]any `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, body)
	}
	var ids []string
	for _, message := range decoded.Messages {
		for _, part := range message.Parts {
			if name, _ := part["name"].(string); part["type"] == "tool_call" && (name == "subagent" || name == "native.subagent") {
				ids = append(ids, part["callId"].(string))
			}
		}
	}
	return ids
}

// TestSubagentToolResultCarriesOnlyASummaryAndTheRouteServesTheSteps is the whole path: a
// real run starts a child; the live stream's tool_result part and the session snapshot the
// browser reloads carry only {agent, steps: <count>}, and the subagent route returns the
// child's completed steps for the call id.
func TestSubagentToolResultCarriesOnlyASummaryAndTheRouteServesTheSteps(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "instructions": "You are the explorer.", "description": "Finds facts."})
	live := f.run(t)
	if !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	// Live: the tool_result part names the summary and no step.
	if !strings.Contains(live, `"subagent":{"agent":"explore","steps":2}`) {
		t.Fatalf("the live stream carries no subagent summary:\n%s", live)
	}
	if strings.Contains(live, `"status":"completed","parts"`) {
		t.Fatalf("the live stream carries subagent steps:\n%s", live)
	}

	served := f.sessionSnapshot(t)
	summaries, results := subagentSnapshotOf(t, served)
	if results != 1 || len(summaries) != 1 {
		t.Fatalf("served tool results = %d, with a summary = %d, want 1 and 1:\n%s", results, len(summaries), served)
	}
	if summaries[0]["agent"] != "explore" || summaries[0]["steps"] != float64(2) || len(summaries[0]) != 2 {
		t.Fatalf("served summary = %v, want only agent explore and 2 steps", summaries[0])
	}
	if strings.Contains(string(served), "the answer is 42\",\"truncated") || strings.Contains(string(served), `"status":"completed","parts"`) {
		t.Fatalf("the served session carries the child's steps:\n%s", served)
	}
	// A second read of the same session is the same bytes.
	if again := f.sessionSnapshot(t); string(again) != string(served) {
		t.Fatalf("a second read of the same session differs:\n%s\n%s", served, again)
	}

	ids := subagentCallIDs(t, served)
	if len(ids) != 1 {
		t.Fatalf("subagent calls = %v", ids)
	}
	status, body := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusOK {
		t.Fatalf("subagent route status = %d, body=%s", status, body)
	}
	var answer struct {
		Subagent struct {
			Steps []struct {
				Status string           `json:"status"`
				Parts  []map[string]any `json:"parts"`
			} `json:"steps"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode %v: %s", err, body)
	}
	if len(answer.Subagent.Steps) != 2 {
		t.Fatalf("route steps = %d, want the child's 2 rounds: %s", len(answer.Subagent.Steps), body)
	}
	pairs := 0
	for _, step := range answer.Subagent.Steps {
		if step.Status != "completed" {
			t.Fatalf("step status = %v, want completed", step.Status)
		}
		calls, results := map[any]bool{}, map[any]bool{}
		for _, part := range step.Parts {
			switch kind := part["type"]; kind {
			case "text", "reasoning":
			case "tool_call":
				calls[part["callId"]] = true
			case "tool_result":
				results[part["callId"]] = true
			default:
				t.Fatalf("part type %v is not text, reasoning, tool_call or tool_result", kind)
			}
			if _, nested := part["subagent"]; nested {
				t.Fatalf("a step part carries a nested summary: %v", part)
			}
		}
		for callID := range calls {
			if !results[callID] {
				t.Fatalf("call %v of a completed step has no paired result: %v", callID, step)
			}
			pairs++
		}
	}
	if pairs == 0 || !strings.Contains(body, "the answer is 42") {
		t.Fatalf("route body has no call/result pair or no answer: %s", body)
	}

	// An unknown call id is a 404 with a message, not an empty snapshot.
	status, body = f.get(t, "/subagents/no-such-call")
	if status != http.StatusNotFound || !strings.Contains(body, "subagent transcript not found") {
		t.Fatalf("unknown call id: status %d, body %s", status, body)
	}
}

// TestSubagentRouteReadsAChildThatIsStillRunning: a child handed off and still going has a
// folder and no completed step yet, so the route answers 200 with no steps (not a 404).
func TestSubagentRouteReadsAChildThatIsStillRunning(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	handoffAndLeaveRunning(t, f)
	ids := subagentCallIDs(t, f.sessionSnapshot(t))
	if len(ids) != 1 {
		t.Fatalf("subagent calls = %v", ids)
	}
	// The child may still be creating its folder when it reaches the provider, so the route
	// is asked until it answers (a 404 then is the child not being there yet).
	deadline := time.Now().Add(10 * time.Second)
	status, body := f.get(t, "/subagents/"+ids[0])
	for status == http.StatusNotFound && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		status, body = f.get(t, "/subagents/"+ids[0])
	}
	if status != http.StatusOK || !strings.Contains(body, `"subagent":{"steps":[]}`) {
		t.Fatalf("running child: status %d, body %s", status, body)
	}
}

// TestSubagentRouteSaysAnUnreadableChildIsAnErrorNotMissing: a child whose transcript exists
// but cannot be read answers 500 (the browser shows the error with a retry), not the 404 of
// an unknown call id, and the body does not name the file.
func TestSubagentRouteSaysAnUnreadableChildIsAnErrorNotMissing(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	if live := f.run(t); !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	ids := subagentCallIDs(t, f.sessionSnapshot(t))
	transcripts, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if len(ids) != 1 || err != nil || len(transcripts) != 1 {
		t.Fatalf("subagent calls %v, child transcripts %v (%v)", ids, transcripts, err)
	}
	path := transcripts[0]
	// The child writes once more (so the cached read is stale), then its file is unreadable.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n")
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("a mode 0 file is still readable here (running as root?)")
	}
	status, body := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusInternalServerError || strings.Contains(body, "not found") || strings.Contains(body, "messages.jsonl") || strings.Contains(body, f.toolDir) {
		t.Fatalf("unreadable child: status %d, body %s; want a 500 that names no file", status, body)
	}
}
