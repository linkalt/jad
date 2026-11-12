package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/webui"
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

	// A child that ended with its last response shown has nothing newer, so the body stays
	// what it was before the bit existed.
	if strings.Contains(body, "mayHaveNewer") {
		t.Fatalf("an ordinary finished child carries the bit: %s", body)
	}

	// An unknown call id is a 404 with a message, not an empty snapshot.
	status, body = f.get(t, "/subagents/no-such-call")
	if status != http.StatusNotFound || !strings.Contains(body, "subagent transcript not found") {
		t.Fatalf("unknown call id: status %d, body %s", status, body)
	}
}

// TestSubagentRouteSaysWhenTheLastStepMayNotBeTheNewestResponse: the bit is in the route
// body once the child's transcript holds a response newer than the last step shown (here a
// record the provider did not complete), and it is not in the summary a tool result carries.
func TestSubagentRouteSaysWhenTheLastStepMayNotBeTheNewestResponse(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	if live := f.run(t); !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	served := f.sessionSnapshot(t)
	ids := subagentCallIDs(t, served)
	transcripts, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if len(ids) != 1 || err != nil || len(transcripts) != 1 {
		t.Fatalf("subagent calls %v, child transcripts %v (%v)", ids, transcripts, err)
	}
	status, before := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusOK || strings.Contains(before, "mayHaveNewer") {
		t.Fatalf("finished child: status %d, body %s", status, before)
	}
	file, err := os.OpenFile(transcripts[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"response.completed","session_turn":9,"session_step":9,"response":{"id":"r9","status":"incomplete","output":[]}}` + "\n")
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	status, after := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusOK || !strings.Contains(after, `"mayHaveNewer":true`) || !strings.Contains(after, "the answer is 42") {
		t.Fatalf("child with a newer refused response: status %d, body %s", status, after)
	}
	// The summary of the result keeps its two keys; the bit is not part of it.
	summaries, _ := subagentSnapshotOf(t, f.sessionSnapshot(t))
	if len(summaries) != 1 || len(summaries[0]) != 2 {
		t.Fatalf("served summaries = %v, want only agent and steps", summaries)
	}
}

// TestSubagentRouteShowsTheNewestStepsOfAChildThatWroteMoreThanTheLimit: through the route, a
// child with more steps than the snapshot keeps shows its newest ones, in order and whole,
// and the body says earlier ones were left out; the summary a tool result carries has no
// such key. A finished child within the limit has no such key either.
func TestSubagentRouteShowsTheNewestStepsOfAChildThatWroteMoreThanTheLimit(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	if live := f.run(t); !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	ids := subagentCallIDs(t, f.sessionSnapshot(t))
	transcripts, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", "messages.jsonl"))
	if len(ids) != 1 || err != nil || len(transcripts) != 1 {
		t.Fatalf("subagent calls %v, child transcripts %v (%v)", ids, transcripts, err)
	}
	status, before := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusOK || strings.Contains(before, "earlierOmitted") || !strings.Contains(before, "the answer is 42") {
		t.Fatalf("finished child within the limit: status %d, body %s", status, before)
	}
	file, err := os.OpenFile(transcripts[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step <= 40; step++ {
		_, err = file.WriteString(completedLine(9, step, fmt.Sprintf("later step %d", step)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	status, after := f.get(t, "/subagents/"+ids[0])
	if status != http.StatusOK || !strings.Contains(after, `"earlierOmitted":true`) || strings.Contains(after, "mayHaveNewer") {
		t.Fatalf("long child: status %d, body %s", status, after)
	}
	var body struct {
		Subagent webui.SubagentSnapshot `json:"subagent"`
	}
	if err := json.Unmarshal([]byte(after), &body); err != nil {
		t.Fatal(err)
	}
	steps := body.Subagent.Steps
	if len(steps) != 32 || steps[0].ID != "9.9" || steps[31].ID != "9.40" || steps[31].Parts[0].Text != "later step 40" || strings.Contains(after, "the answer is 42") {
		t.Fatalf("steps = %d, first %q, last %q: want the newest 32 in order", len(steps), steps[0].ID, steps[len(steps)-1].ID)
	}
	summaries, _ := subagentSnapshotOf(t, f.sessionSnapshot(t))
	if len(summaries) != 1 || len(summaries[0]) != 2 {
		t.Fatalf("served summaries = %v, want only agent and steps", summaries)
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
	// The handed-off child is going: the body says so, beside the steps it has (none yet).
	if status != http.StatusOK || !strings.Contains(body, `"subagent":{"steps":[],"running":true}`) {
		t.Fatalf("running child: status %d, body %s", status, body)
	}
}

// TestHandedOffChildWithNoStepHasASummaryLiveAndOnReplay: a call that handed the child off
// before its first completed response still has a transcript to open: the live tool result
// and the replayed session both carry {steps: 0, omitsLastText} (a hand-off result does not
// name the agent, and this call's arguments do not either, so there is none to add).
func TestHandedOffChildWithNoStepHasASummaryLiveAndOnReplay(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.handoffOnly = true
	f.provider.childBlocks = true
	f.provider.childEntered = make(chan struct{}, 1)
	f.provider.childCancelled = make(chan struct{}, 1)
	f.provider.childRelease = make(chan struct{})
	t.Cleanup(func() { close(f.provider.childRelease) })

	live := f.run(t)
	if !strings.Contains(live, `"subagent":{"steps":0,"omitsLastText":true}`) {
		t.Fatalf("the live tool result of a handed-off child with no step has no zero-step summary:\n%s", live)
	}
	summaries, results := subagentSnapshotOf(t, f.sessionSnapshot(t))
	if results != 1 || len(summaries) != 1 {
		t.Fatalf("replayed tool results = %d, with a summary = %d, want 1 and 1", results, len(summaries))
	}
	if got := summaries[0]; len(got) != 2 || got["steps"] != float64(0) || got["omitsLastText"] != true {
		t.Fatalf("replayed summary = %v, want steps 0 and omitsLastText true", got)
	}
}

// routeStepIDs decodes a subagent route body: the step ids and whether it says running.
func routeStepIDs(t *testing.T, body string) (ids []string, running bool) {
	t.Helper()
	var decoded struct {
		Subagent struct {
			Steps []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"steps"`
			Running bool `json:"running"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode %v: %s", err, body)
	}
	for _, step := range decoded.Subagent.Steps {
		if step.Status != "completed" {
			t.Fatalf("step %s has status %q: %s", step.ID, step.Status, body)
		}
		ids = append(ids, step.ID)
	}
	return ids, decoded.Subagent.Running
}

// TestSubagentRouteFillsInWhileTheParentCallIsUnsettled: the parent's call waits for its
// child (no hand-off) while the child is let go one response at a time. The route, asked at
// each pause, returns the completed steps so far (none, one, then two) with running true,
// never the response being written; once the child ends it returns every step and no
// running key, and that body is what the route answered before the flag existed.
func TestSubagentRouteFillsInWhileTheParentCallIsUnsettled(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json"})
	f.provider.childRounds = 2
	f.provider.childGate = make(chan struct{}, 8)
	f.provider.childAtGate = make(chan struct{}, 8)
	t.Cleanup(func() { close(f.provider.childGate) })

	finished := make(chan string, 1)
	go func() {
		payload, _ := json.Marshal(map[string]string{"input": "ask the explorer"})
		request, _ := http.NewRequest(http.MethodPost, f.apiURL+"/api/sessions/local:demo/run", strings.NewReader(string(payload)))
		request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			finished <- "error: " + err.Error()
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		finished <- string(body)
	}()
	atGate := func(round int) {
		t.Helper()
		select {
		case <-f.provider.childAtGate:
		case <-time.After(10 * time.Second):
			t.Fatalf("child request %d never reached the provider", round)
		}
	}
	// route asks the route for the call the parent is waiting on.
	route := func() (ids []string, running bool, body string) {
		t.Helper()
		status, body := f.get(t, "/subagents/call-sub")
		if status != http.StatusOK {
			t.Fatalf("route status = %d, body = %s", status, body)
		}
		ids, running = routeStepIDs(t, body)
		return ids, running, body
	}
	for round, want := range [][]string{nil, {"1.1"}, {"1.1", "1.2"}} {
		atGate(round + 1)
		select {
		case body := <-finished:
			t.Fatalf("the parent's call settled while the child waited at its request %d:\n%s", round+1, body)
		default:
		}
		ids, running, body := route()
		if !running || strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("at child request %d the route gave steps %v running %v, want %v running: %s", round+1, ids, running, want, body)
		}
		// Asked again with nothing changed, it says the same (a cache hit, not a new read).
		if _, _, again := route(); again != body {
			t.Fatalf("a second read with no new response differs:\n%s\n%s", body, again)
		}
		f.provider.childGate <- struct{}{}
	}
	// The third request, let through in the last round above, is the child's answer.
	var live string
	select {
	case live = <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not end")
	}
	if !strings.Contains(live, "event: completed") {
		t.Fatalf("run did not complete:\n%s", live)
	}
	ids, running, body := route()
	if running || len(ids) != 3 || strings.Contains(body, `"running"`) {
		t.Fatalf("finished child: steps %v running %v, body %s", ids, running, body)
	}
	// A finished child's body is byte for byte what it was before the flag: the snapshot
	// alone under "subagent".
	transcripts, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "call-sub-*"))
	if err != nil || len(transcripts) != 1 {
		t.Fatalf("child folders %v (%v)", transcripts, err)
	}
	legacy, err := json.Marshal(struct {
		Subagent *webui.SubagentSnapshot `json:"subagent"`
	}{readSubagentSnapshot(context.Background(), filepath.Join(f.toolDir, ".subagents"), webui.SubagentCall{CallID: "call-sub"}, defaultSubagentSnapshotLimits)})
	if err != nil || strings.TrimSpace(body) != string(legacy) {
		t.Fatalf("finished child's body changed:\n%s\n%s", body, legacy)
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
