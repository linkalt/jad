package webui

import (
	"encoding/json"
	"testing"
)

func TestNormalizeTranscriptGroupsToolStepsAndResults(t *testing.T) {
	records := []TranscriptRecord{
		{
			Offset:    0,
			Prefix:    "o",
			Cursor:    1,
			HasCursor: true,
			Raw:       json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`),
		},
		{
			Offset:    40,
			Prefix:    "o",
			Cursor:    1,
			HasCursor: true,
			Raw:       json.RawMessage(`{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"resp-1","output":[{"type":"message","content":[{"type":"output_text","text":"before "}]},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}]}}`),
		},
		{
			Offset:    240,
			Prefix:    "o",
			Cursor:    1,
			HasCursor: true,
			Raw:       json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"{\"status\":\"ok\"}\nresult"}`),
		},
		{
			Offset:    300,
			Prefix:    "o",
			Cursor:    1,
			HasCursor: true,
			Raw:       json.RawMessage(`{"type":"response.completed","session_turn":1,"session_step":2,"response":{"id":"resp-2","output":[{"type":"message","content":[{"type":"output_text","text":"after"}]}]}}`),
		},
	}
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	assistant := messages[1]
	if assistant.ID != "resp-1" || assistant.Text != "before after" || len(assistant.Parts) != 4 {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.Parts[1].Type != "tool_call" || assistant.Parts[2].Type != "tool_result" || assistant.Parts[2].Status != "success" {
		t.Fatalf("assistant tool parts = %+v", assistant.Parts)
	}
	if len(assistant.Steps) != 2 || !assistant.Steps[0].HasTools || assistant.Steps[1].HasTools || assistant.Steps[0].EndPart != 3 || assistant.Steps[1].StartPart != 3 {
		t.Fatalf("assistant steps = %+v", assistant.Steps)
	}
}

func TestCapMessagesUsesUTF8ByteBoundaryAndOnlyAddsCursorWhenCapped(t *testing.T) {
	cursor := int64(7)
	messages := []Message{{
		ID: "m1", Sender: "assistant", Text: "é🙂x", Parts: []Part{{Type: "text", Text: "é🙂x"}}, Cursor: &cursor,
	}}
	capped := CapMessages(messages, 3, nil)
	if got := capped[0].Parts[0].Text; got != "é" {
		t.Fatalf("capped text = %q, want UTF-8-safe prefix", got)
	}
	if capped[0].Parts[0].Truncated == nil || capped[0].Parts[0].Truncated.Bytes != 7 || capped[0].Cursor == nil || *capped[0].Cursor != cursor {
		t.Fatalf("capped message metadata = %+v", capped[0])
	}
	uncapped := CapMessages(messages, 0, nil)
	if uncapped[0].Cursor != nil {
		t.Fatalf("uncapped message unexpectedly exposes cursor %d", *uncapped[0].Cursor)
	}
}

func TestIncrementsEmitsDeltaPartAndSnapshotEvents(t *testing.T) {
	previous := Message{ID: "m", Status: "streaming", Parts: []Part{{Type: "text", Text: "a"}}}
	next := Message{ID: "m", Status: "streaming", Parts: []Part{{Type: "text", Text: "ab"}}}
	updates := Increments(previous, next)
	if len(updates) != 1 || updates[0].Name != "ui.delta" || updates[0].Data.(map[string]any)["append"] != "b" {
		t.Fatalf("text increments = %+v", updates)
	}

	toolPrevious := Message{ID: "m", Status: "streaming", Parts: []Part{{Type: "tool_call", CallID: "c", Name: "f", Arguments: stringPointer("{"), Partial: true}}}
	toolNext := Message{ID: "m", Status: "streaming", Parts: []Part{{Type: "tool_call", CallID: "c", Name: "f", Arguments: stringPointer("{\"x"), Partial: true}}}
	updates = Increments(toolPrevious, toolNext)
	if len(updates) != 1 || updates[0].Name != "ui.delta" {
		t.Fatalf("tool argument increments = %+v", updates)
	}

	changed := Message{ID: "m", Status: "streaming", Parts: []Part{{Type: "reasoning", Text: "thinking"}}}
	updates = Increments(previous, changed)
	if len(updates) != 1 || updates[0].Name != "ui.part" {
		t.Fatalf("part replacement = %+v", updates)
	}
	changed.Status = "complete"
	updates = Increments(previous, changed)
	if len(updates) != 1 || updates[0].Name != "ui.message" {
		t.Fatalf("status transition = %+v", updates)
	}
}

func stringPointer(value string) *string { return &value }

// A reasoning item carries its text in `summary` (OpenAI) or in `content` as
// reasoning_text (vLLM-style providers, the Anthropic adapter). Both must show.
func TestNormalizeTranscriptKeepsReasoningFromSummaryAndContent(t *testing.T) {
	cases := map[string]struct{ item, want string }{
		"summary":                     {`{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"S"}]}`, "S"},
		"content":                     {`{"type":"reasoning","id":"rs","summary":[],"content":[{"type":"reasoning_text","text":"C"}]}`, "C"},
		"content without summary key": {`{"type":"reasoning","id":"rs","content":[{"type":"reasoning_text","text":"C"},{"type":"reasoning_text","text":"D"}]}`, "CD"},
		"summary wins over content":   {`{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"C"}]}`, "S"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			raw := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"r1","output":[` + test.item + `,{"type":"message","content":[{"type":"output_text","text":"ans"}]}]}}`
			messages, err := NormalizeTranscript([]TranscriptRecord{{Offset: 0, Prefix: "o", Cursor: 1, HasCursor: true, Raw: json.RawMessage(raw)}})
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || messages[0].ReasoningContent != test.want || messages[0].Text != "ans" {
				t.Fatalf("messages = %+v, want reasoning %q and text ans", messages, test.want)
			}
			if len(messages[0].Parts) != 2 || messages[0].Parts[0].Type != "reasoning" {
				t.Fatalf("parts = %+v, want reasoning then text", messages[0].Parts)
			}
		})
	}
}

// The live message must keep the thinking when the completed response arrives
// carrying it only as content, with no streamed deltas.
func TestStreamingRunCompleteKeepsContentReasoning(t *testing.T) {
	run := NewStreamingRun("turn", "now")
	item := `{"type":"reasoning","id":"rs","summary":[],"content":[{"type":"reasoning_text","text":"thinking hard"}]}`
	done := `{"type":"response.completed","response":{"id":"r1","output":[` + item + `,{"type":"message","content":[{"type":"output_text","text":"ans"}]}]}}`
	final := run.Complete(json.RawMessage(done))
	if final.ReasoningContent != "thinking hard" || final.Text != "ans" {
		t.Fatalf("final = %+v", final)
	}
}

func subagentOutput(extra string) string {
	return "{\"id\":\"c1\",\"status\":\"ok\",\"summary\":\"Agent explore finished\"}\n\n--- INFORMATION ---\nagent: explore\nrounds: 2\n" + extra + "\n--- CONTENT token=t encoding=utf8 ---\nthe answer\n--- END CONTENT token=t ---\n"
}

func toolMessage(name, arguments, output string) Message {
	return Message{ID: "m", Sender: "assistant", Parts: []Part{
		{Type: "tool_call", CallID: "c1", Name: name, Arguments: &arguments},
		{Type: "tool_result", CallID: "c1", Output: output, Status: "success"},
	}}
}

func TestSubagentBuilderKeepsCompletedStepsAsTextReasoningAndToolCalls(t *testing.T) {
	builder := NewSubagentBuilder("explore", SubagentLimits{})
	lines := []string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"the task"}]}`,
		`{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"r1","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"function_call","call_id":"k1","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"web_search_call","id":"w1","status":"completed"}]}}`,
		`{"type":"function_call_output","call_id":"k1","output":"big tool result"}`,
		`{"type":"response.completed","session_turn":1,"session_step":2,"response":{"id":"r2","status":"in_progress","output":[{"type":"message","content":[{"type":"output_text","text":"not done"}]}]}}`,
		`{"type":"response.completed","session_turn":1,"session_step":3,"response":{"id":"r3","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"final"}]}]}}`,
		`{"type":"response.completed","session_turn":1,"session_step":4,"response":{"id":"r4","output":[]}}`,
	}
	for index, line := range lines {
		builder.Add(int64(index*100), []byte(line))
	}
	snapshot := builder.Snapshot()
	if snapshot == nil || snapshot.Agent != "explore" || len(snapshot.Steps) != 2 {
		t.Fatalf("snapshot = %+v, want 2 completed steps", snapshot)
	}
	first, second := snapshot.Steps[0], snapshot.Steps[1]
	if first.ID != "1.1" || second.ID != "1.3" || first.Status != SubagentStepCompleted || second.Status != SubagentStepCompleted {
		t.Fatalf("steps = %+v", snapshot.Steps)
	}
	if len(first.Parts) != 2 || first.Parts[0].Type != "reasoning" || first.Parts[0].Text != "thinking" || first.Parts[1].Type != "tool_call" || first.Parts[1].Name != "read" || *first.Parts[1].Arguments != `{"path":"a"}` {
		t.Fatalf("first step parts = %+v (no tool_result, no hosted_tool)", first.Parts)
	}
	if len(second.Parts) != 1 || second.Parts[0].Text != "final" {
		t.Fatalf("second step parts = %+v", second.Parts)
	}
	if NewSubagentBuilder("x", SubagentLimits{}).Snapshot() != nil {
		t.Fatal("a builder that saw no step produced a snapshot")
	}
}

func TestSubagentBuilderStopsAtItsLimits(t *testing.T) {
	line := func(step int, text string) []byte {
		return []byte(`{"type":"response.completed","session_turn":1,"session_step":` + string(rune('0'+step)) + `,"response":{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"` + text + `"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"more"}]}]}}`)
	}
	builder := NewSubagentBuilder("", SubagentLimits{MaxSteps: 2, MaxPartsPerStep: 1, MaxPartBytes: 3, MaxTotalBytes: 100})
	if !builder.Add(0, line(1, "abcdef")) || builder.Add(1, line(2, "xyz")) {
		t.Fatal("Add must report false exactly when the step limit is reached")
	}
	if builder.Add(2, line(3, "late")) {
		t.Fatal("a finished builder accepted a line")
	}
	snapshot := builder.Snapshot()
	if len(snapshot.Steps) != 2 || len(snapshot.Steps[0].Parts) != 1 || snapshot.Steps[0].Parts[0].Text != "abc" || snapshot.Steps[0].Parts[0].Truncated == nil || snapshot.Steps[0].Parts[0].Truncated.Bytes != 6 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	budget := NewSubagentBuilder("", SubagentLimits{MaxPartBytes: 100, MaxTotalBytes: 5})
	budget.Add(0, line(1, "abcdefgh"))
	budget.Add(1, line(2, "zzz"))
	if got := budget.Snapshot(); got == nil || len(got.Steps) != 1 || got.Steps[0].Parts[0].Text != "abcde" {
		t.Fatalf("total budget: %+v", got)
	}
}

func TestAttachSubagentsOnlyTouchesSubagentToolResults(t *testing.T) {
	snapshot := &SubagentSnapshot{Agent: "explore", Steps: []SubagentStep{{ID: "1.1", Status: "completed", Parts: []Part{{Type: "text", Text: "x"}}}}}
	var asked []SubagentCall
	lookup := func(call SubagentCall) *SubagentSnapshot {
		asked = append(asked, call)
		return snapshot
	}
	output := subagentOutput("transcript: /store/c1-0a1b2c3d/messages.jsonl\n")
	messages := []Message{
		toolMessage("subagent", `{"task":"t"}`, output),
		toolMessage("native.subagent", `{"agent":"fromargs"}`, "plain output"),
		toolMessage("profile_message", `{}`, output),
		toolMessage("read", `{}`, output),
		{ID: "orphan", Parts: []Part{{Type: "tool_result", CallID: "c1", Output: output}}},
	}
	attached := AttachSubagents(messages, lookup)
	if attached[0].Parts[1].Subagent != snapshot || attached[1].Parts[1].Subagent != snapshot {
		t.Fatalf("subagent results got no snapshot: %+v %+v", attached[0].Parts[1], attached[1].Parts[1])
	}
	for _, index := range []int{2, 3, 4} {
		for _, part := range attached[index].Parts {
			if part.Subagent != nil {
				t.Errorf("message %d (%s) carries a snapshot", index, attached[index].ID)
			}
		}
	}
	if len(asked) != 2 || asked[0] != (SubagentCall{CallID: "c1", Agent: "explore", Transcript: "/store/c1-0a1b2c3d/messages.jsonl"}) || asked[1].Agent != "fromargs" || asked[1].Transcript != "" {
		t.Fatalf("lookups = %+v", asked)
	}
	// Nothing the caller passed in was edited: it may be a cached block.
	if messages[0].Parts[1].Subagent != nil || messages[1].Parts[1].Subagent != nil {
		t.Fatal("AttachSubagents edited its input")
	}
	// A lookup with nothing to show leaves the messages as they are.
	if same := AttachSubagents(messages, func(SubagentCall) *SubagentSnapshot { return nil }); &same[0] != &messages[0] {
		t.Fatal("no snapshot still copied the messages")
	}
}

func TestCapMessagesCutsSubagentTextAndKeepsTheSnapshotOtherwise(t *testing.T) {
	long := "é🙂xyz"
	arguments := "{\"a\":\"" + long + "\"}"
	snapshot := &SubagentSnapshot{Agent: "a", Steps: []SubagentStep{{ID: "1.1", Status: "completed", Parts: []Part{
		{Type: "text", Text: long},
		{Type: "tool_call", CallID: "k", Name: "n", Arguments: &arguments},
	}}}}
	messages := []Message{{ID: "m", Sender: "assistant", Parts: []Part{{Type: "tool_result", CallID: "c", Output: "ok", Subagent: snapshot}}}}
	cursor := int64(4)
	capped := CapMessages(messages, 3, map[string]int64{"m": cursor})
	cut := capped[0].Parts[0].Subagent
	if cut == snapshot || cut.Agent != "a" || cut.Steps[0].Parts[0].Text != "é" || cut.Steps[0].Parts[0].Truncated == nil || cut.Steps[0].Parts[0].Truncated.Bytes != len(long) {
		t.Fatalf("capped snapshot = %+v", cut)
	}
	if got := *cut.Steps[0].Parts[1].Arguments; len(got) > 3 || cut.Steps[0].Parts[1].Truncated == nil {
		t.Fatalf("capped arguments = %q", got)
	}
	if capped[0].Cursor == nil || *capped[0].Cursor != cursor {
		t.Fatalf("a cut snapshot must give the message its cursor: %+v", capped[0].Cursor)
	}
	// The shared snapshot itself is untouched.
	if snapshot.Steps[0].Parts[0].Text != long || snapshot.Steps[0].Parts[0].Truncated != nil {
		t.Fatalf("CapMessages edited the shared snapshot: %+v", snapshot)
	}
	// Under the cap, or with no cap, the very same snapshot is served.
	for _, limit := range []int{0, 1 << 20} {
		if got := CapMessages(messages, limit, nil)[0].Parts[0].Subagent; got != snapshot {
			t.Fatalf("limit %d replaced the snapshot", limit)
		}
	}
}

func TestIncrementsReportsAChangedSubagentSnapshot(t *testing.T) {
	part := Part{Type: "tool_result", CallID: "c", Output: "ok"}
	with := part
	with.Subagent = &SubagentSnapshot{Steps: []SubagentStep{{ID: "1.1", Status: "completed", Parts: []Part{{Type: "text", Text: "x"}}}}}
	events := Increments(Message{ID: "m", Parts: []Part{part}}, Message{ID: "m", Parts: []Part{with}})
	if len(events) != 1 || events[0].Name != "ui.part" {
		t.Fatalf("events = %+v, want one ui.part for the part that gained a snapshot", events)
	}
	if events := Increments(Message{ID: "m", Parts: []Part{with}}, Message{ID: "m", Parts: []Part{with}}); len(events) != 0 {
		t.Fatalf("an unchanged snapshot produced %+v", events)
	}
}

func TestStreamingRunToolCallNamesTheToolOfAResult(t *testing.T) {
	run := NewStreamingRun("m", "")
	if _, ok := run.ToolCall("c1"); ok {
		t.Fatal("an empty run knows a call")
	}
	run.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"r","output":[{"type":"function_call","call_id":"c1","name":"subagent","arguments":"{}"}]}}`))
	call, ok := run.ToolCall("c1")
	if !ok || call.Name != "subagent" {
		t.Fatalf("ToolCall = %+v, %v", call, ok)
	}
}
