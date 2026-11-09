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

// TestLiveSubagentResultSaysWhetherItOmitsTheLastText: a result that handed the child off
// (it names the run's state next to the run id) or cut its answer (it names truncated) is
// published with a summary marked as omitting the child's last text, so the browser keeps
// that text; the result of a child that ended with its answer whole is not marked. The
// replay sets the summary with the same function (webui.SubagentSummaryOf).
func TestLiveSubagentResultSaysWhetherItOmitsTheLastText(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	var published []webui.Message
	emitter := newMessageStreamEmitter(0, func(message webui.Message) error { published = append(published, message); return nil })
	streaming := webui.NewStreamingRun("turn", "now")
	streaming.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"r","output":[
		{"type":"function_call","call_id":"handed","name":"subagent","arguments":"{}"},
		{"type":"function_call","call_id":"ended","name":"subagent","arguments":"{}"},
		{"type":"function_call","call_id":"cut","name":"subagent","arguments":"{}"}]}}`))
	outputs := map[string]string{
		"handed": "{\"id\":\"handed\",\"status\":\"ok\",\"summary\":\"Agent explore is still running (run sub_0a1b2c3d)\"}\n\n--- INFORMATION ---\nrun: sub_0a1b2c3d\nstate: running\nelapsed_ms: 5\nlimit_ms: 600000\n",
		"ended":  "{\"id\":\"ended\",\"status\":\"ok\",\"summary\":\"Agent explore finished\"}\n\n--- INFORMATION ---\nagent: explore\nrounds: 2\n\n--- CONTENT token=t encoding=utf8 ---\nthe answer\n--- END CONTENT token=t ---\n",
		"cut":    "{\"id\":\"cut\",\"status\":\"ok\",\"summary\":\"Agent explore finished\"}\n\n--- INFORMATION ---\nagent: explore\nrounds: 2\ntruncated: true\n\n--- CONTENT token=t encoding=utf8 ---\nthe head of the ans\n--- END CONTENT token=t ---\n",
	}
	runner := &snapshotRunner{}
	for _, callID := range []string{"handed", "ended", "cut"} {
		item, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": outputs[callID]})
		publishToolResult(run, streaming, emitter, item, runner, nil)
	}
	summaries := map[string]*webui.SubagentSummary{}
	for _, part := range published[len(published)-1].Parts {
		if part.Type == "tool_result" {
			summaries[part.CallID] = part.Subagent
		}
	}
	if got := summaries["handed"]; got == nil || *got != (webui.SubagentSummary{Steps: 1, OmitsLastText: true}) {
		t.Fatalf("handed-off result summary = %+v", got)
	}
	if got := summaries["cut"]; got == nil || *got != (webui.SubagentSummary{Agent: "explore", Steps: 1, OmitsLastText: true}) {
		t.Fatalf("cut finished result summary = %+v", got)
	}
	if got := summaries["ended"]; got == nil || *got != (webui.SubagentSummary{Agent: "explore", Steps: 1}) {
		t.Fatalf("finished result summary = %+v", got)
	}
}

// emptyRunner is a session whose child has completed no step yet.
type emptyRunner struct{ snapshotRunner }

func (r *emptyRunner) SubagentSnapshot(call webui.SubagentCall) *webui.SubagentSnapshot {
	r.asked = append(r.asked, call)
	return nil
}

// TestLiveHandedOffResultOfAChildWithNoStepCarriesAZeroStepSummary: a call that handed the
// child off before it completed a response is published with {agent, steps: 0}, so the
// browser has a transcript to open at once; a synchronous result of a child with no step
// still has no summary.
func TestLiveHandedOffResultOfAChildWithNoStepCarriesAZeroStepSummary(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	var published []webui.Message
	emitter := newMessageStreamEmitter(0, func(message webui.Message) error { published = append(published, message); return nil })
	streaming := webui.NewStreamingRun("turn", "now")
	streaming.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"r","output":[
		{"type":"function_call","call_id":"handed","name":"subagent","arguments":"{\"agent\":\"explore\"}"},
		{"type":"function_call","call_id":"sync","name":"subagent","arguments":"{\"agent\":\"explore\"}"}]}}`))
	outputs := map[string]string{
		"handed": "{\"id\":\"handed\",\"status\":\"ok\",\"summary\":\"Agent explore is still running (run sub_0a1b2c3d)\"}\n\n--- INFORMATION ---\nrun: sub_0a1b2c3d\nstate: running\nelapsed_ms: 5\nlimit_ms: 600000\n",
		"sync":   "{\"id\":\"sync\",\"status\":\"ok\",\"summary\":\"Agent explore finished\"}\n\n--- INFORMATION ---\nagent: explore\nrounds: 0\n",
	}
	runner := &emptyRunner{}
	for _, callID := range []string{"handed", "sync"} {
		item, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": outputs[callID]})
		publishToolResult(run, streaming, emitter, item, runner, nil)
	}
	summaries := map[string]*webui.SubagentSummary{}
	for _, part := range published[len(published)-1].Parts {
		if part.Type == "tool_result" {
			summaries[part.CallID] = part.Subagent
		}
	}
	if got := summaries["handed"]; got == nil || *got != (webui.SubagentSummary{Agent: "explore", Steps: 0, OmitsLastText: true}) {
		t.Fatalf("handed-off result summary = %+v", got)
	}
	if summaries["sync"] != nil {
		t.Fatalf("a synchronous result of a child with no step has a summary: %+v", summaries["sync"])
	}
}
