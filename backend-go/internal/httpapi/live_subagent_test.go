package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/webui"
)

type snapshotRunner struct{ asked []webui.SubagentCall }

func (*snapshotRunner) Run(context.Context, json.RawMessage, session.RunOptions) (session.RunResult, error) {
	return session.RunResult{}, nil
}

func (r *snapshotRunner) SubagentSnapshot(call webui.SubagentCall) *webui.SubagentSnapshot {
	r.asked = append(r.asked, call)
	return &webui.SubagentSnapshot{Agent: call.Agent, Steps: []webui.SubagentStep{{ID: "1.1", Status: "completed", Parts: []webui.Part{{Type: "text", Text: "child"}}}}}
}

// TestLiveSubagentResultCarriesTheSummaryAndNoOtherToolDoes: the live publish asks the
// run's session for the child's steps only for the subagent tool, found by the name of
// the call the result belongs to; profile_message prints the same transcript line and
// gets none, and a session that cannot snapshot publishes the result as it is.
func TestLiveSubagentResultCarriesTheSummaryAndNoOtherToolDoes(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	var published []webui.Message
	emitter := newMessageStreamEmitter(0, func(message webui.Message) error { published = append(published, message); return nil })
	streaming := webui.NewStreamingRun("turn", "now")
	streaming.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"r","output":[
		{"type":"function_call","call_id":"sub","name":"subagent","arguments":"{\"agent\":\"fromargs\"}"},
		{"type":"function_call","call_id":"msg","name":"profile_message","arguments":"{}"},
		{"type":"function_call","call_id":"sub2","name":"subagent","arguments":"{}"}]}}`))
	output := "{\"id\":\"x\",\"status\":\"ok\",\"summary\":\"done\"}\n\n--- INFORMATION ---\nagent: explore\ntranscript: /store/sub-0a1b2c3d/messages.jsonl\n"
	item := func(callID string) json.RawMessage {
		encoded, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": output})
		return encoded
	}
	runner := &snapshotRunner{}
	publishToolResult(run, streaming, emitter, item("sub"), runner, nil)
	publishToolResult(run, streaming, emitter, item("msg"), runner, nil)
	publishToolResult(run, streaming, emitter, item("sub2"), nil, nil)
	publishToolResult(run, streaming, emitter, item("unknown-call"), runner, nil)

	final := published[len(published)-1]
	bySnapshot := map[string]*webui.SubagentSummary{}
	for _, part := range final.Parts {
		if part.Type == "tool_result" {
			bySnapshot[part.CallID] = part.Subagent
		}
	}
	if got := bySnapshot["sub"]; got == nil || *got != (webui.SubagentSummary{Agent: "explore", Steps: 1}) {
		t.Fatalf("subagent result summary = %+v", got)
	}
	if bySnapshot["msg"] != nil || bySnapshot["sub2"] != nil || bySnapshot["unknown-call"] != nil {
		t.Fatalf("snapshots on results that must have none: %+v", bySnapshot)
	}
	if len(runner.asked) != 1 || runner.asked[0] != (webui.SubagentCall{CallID: "sub", Agent: "explore", Transcript: "/store/sub-0a1b2c3d/messages.jsonl"}) {
		t.Fatalf("session asked %+v", runner.asked)
	}
}
