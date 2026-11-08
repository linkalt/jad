package webui

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestSubagentBuilderKeepsCompletedStepsWithTheirPairedResults(t *testing.T) {
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
	if len(first.Parts) != 3 || first.Parts[0].Type != "reasoning" || first.Parts[0].Text != "thinking" || first.Parts[1].Type != "tool_call" || first.Parts[1].Name != "read" || *first.Parts[1].Arguments != `{"path":"a"}` {
		t.Fatalf("first step parts = %+v (reasoning, call, result; no hosted_tool)", first.Parts)
	}
	if result := first.Parts[2]; result.Type != "tool_result" || result.CallID != "k1" || result.Output != "big tool result" || result.Status != "unknown" {
		t.Fatalf("the call's result must follow its step in the conversation's shape: %+v", result)
	}
	if len(second.Parts) != 1 || second.Parts[0].Text != "final" {
		t.Fatalf("second step parts = %+v", second.Parts)
	}
	// The child's last record is a response with nothing to keep (step 4): it is newer than
	// the last step shown and the snapshot cannot tell it from a final text it left out, so
	// it says so. Showing a text twice is the price; a text shown nowhere is not paid.
	if !snapshot.MayHaveNewer {
		t.Fatalf("a refused response after the last step must set MayHaveNewer: %+v", snapshot)
	}
	if NewSubagentBuilder("x", SubagentLimits{}).Snapshot() != nil {
		t.Fatal("a builder that saw no step produced a snapshot")
	}
}

func responseLine(step int, output string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.completed","session_turn":1,"session_step":%d,"response":{"id":"r%d","output":[%s]}}`, step, step, output))
}

func callItem(id string) string {
	return `{"type":"function_call","call_id":"` + id + `","name":"read","arguments":"{}"}`
}

func outputLine(id, output string) []byte {
	raw, _ := json.Marshal(output)
	return []byte(`{"type":"function_call_output","call_id":"` + id + `","output":` + string(raw) + `}`)
}

func addAll(builder *SubagentBuilder, lines ...[]byte) {
	for index, line := range lines {
		builder.Add(int64(index*1000), line)
	}
}

func partTypes(parts []Part) string {
	types := make([]string, len(parts))
	for index, part := range parts {
		types[index] = part.Type + ":" + part.CallID
	}
	return strings.Join(types, ",")
}

func TestSubagentBuilderPairsEachCallWithItsOwnResult(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{})
	addAll(builder,
		responseLine(1, callItem("a")+","+callItem("b")),
		// Results may arrive in another order than the calls.
		outputLine("b", "second"),
		outputLine("a", "first"),
		// A result of a call nobody made, and a second result of a paired call, are ignored.
		outputLine("zzz", "stray"),
		outputLine("a", "again"),
		responseLine(2, `{"type":"message","content":[{"type":"output_text","text":"done"}]}`),
	)
	snapshot := builder.Snapshot()
	if snapshot == nil || len(snapshot.Steps) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if got := partTypes(snapshot.Steps[0].Parts); got != "tool_call:a,tool_call:b,tool_result:b,tool_result:a" {
		t.Fatalf("step 1 parts = %s", got)
	}
	if snapshot.Steps[0].Parts[2].Output != "second" || snapshot.Steps[0].Parts[3].Output != "first" {
		t.Fatalf("results are not the ones of their calls: %+v", snapshot.Steps[0].Parts)
	}
}

// A step is completed when its response is written and its calls have their results. The
// last step of a child that is still running a tool is not reported, and nothing claims
// it is complete.
func TestSubagentBuilderLeavesOutAStepThatStillWaitsForAResult(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{})
	addAll(builder,
		responseLine(1, callItem("a")),
		outputLine("a", "ok"),
		responseLine(2, callItem("b")+","+callItem("c")),
		outputLine("b", "half"),
	)
	snapshot := builder.Snapshot()
	if snapshot == nil || len(snapshot.Steps) != 1 || snapshot.Steps[0].ID != "1.1" {
		t.Fatalf("a running tool's step must not be reported: %+v", snapshot)
	}
	if !snapshot.MayHaveNewer {
		t.Fatalf("a withheld step is a newer response than the last one shown: %+v", snapshot)
	}
	// Once the child moves on past it without a result (a lost record), the step is
	// reported with the results it has: the child's own next response shows it finished.
	addAll(builder, responseLine(3, `{"type":"message","content":[{"type":"output_text","text":"end"}]}`))
	snapshot = builder.Snapshot()
	if len(snapshot.Steps) != 3 || partTypes(snapshot.Steps[1].Parts) != "tool_call:b,tool_call:c,tool_result:b" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.MayHaveNewer {
		t.Fatalf("the last step shown is the newest response again: %+v", snapshot)
	}
}

// A tool output can quote the markers (the child read a file that mentions them); the
// record's own type decides what a line is.
func TestSubagentBuilderReadsRecordsByTypeNotByWordsInThem(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{})
	addAll(builder,
		responseLine(1, callItem("a")),
		outputLine("a", `{"type":"response.completed"} function_call_output "message"`),
		responseLine(2, `{"type":"message","content":[{"type":"output_text","text":"say function_call_output"}]}`),
	)
	snapshot := builder.Snapshot()
	if snapshot == nil || len(snapshot.Steps) != 2 || len(snapshot.Steps[0].Parts) != 2 || !strings.Contains(snapshot.Steps[0].Parts[1].Output, "function_call_output") {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestSubagentBuilderCutsResultsAndDropsTheirImages(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{MaxResultBytes: 4, MaxTotalBytes: 1000})
	addAll(builder,
		responseLine(1, callItem("a")),
		outputLine("a", "abcdefgh"),
	)
	result := builder.Snapshot().Steps[0].Parts[1]
	if result.Output != "abcd" || result.Truncated == nil || result.Truncated.Bytes != 8 || result.Images != nil || result.InlineImages != 0 || result.Subagent != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestSubagentBuilderKeepsNoResultOfACallOverThePartLimit(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{MaxPartsPerStep: 1})
	addAll(builder,
		responseLine(1, callItem("a")+","+callItem("b")),
		outputLine("b", "dropped with its call"),
		outputLine("a", "kept"),
	)
	if got := partTypes(builder.Snapshot().Steps[0].Parts); got != "tool_call:a,tool_result:a" {
		t.Fatalf("parts = %s", got)
	}
}

func TestSubagentBuilderStopsWhenResultsUseTheByteBudget(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{MaxResultBytes: 100, MaxTotalBytes: 14})
	addAll(builder,
		responseLine(1, callItem("a")+","+callItem("b")),
		outputLine("a", "0123456789"),
		outputLine("b", "never fits"),
	)
	snapshot := builder.Snapshot()
	if snapshot == nil || len(snapshot.Steps) != 1 || builder.Add(0, responseLine(2, callItem("c"))) {
		t.Fatalf("the budget must end the snapshot: %+v", snapshot)
	}
	if got := partTypes(snapshot.Steps[0].Parts); got != "tool_call:a,tool_call:b,tool_result:a" {
		t.Fatalf("parts = %s", got)
	}
}

func TestSubagentBuilderStopsAtItsLimits(t *testing.T) {
	line := func(step int, text string) []byte {
		return []byte(`{"type":"response.completed","session_turn":1,"session_step":` + string(rune('0'+step)) + `,"response":{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"` + text + `"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"more"}]}]}}`)
	}
	builder := NewSubagentBuilder("", SubagentLimits{MaxSteps: 2, MaxPartsPerStep: 1, MaxPartBytes: 3, MaxTotalBytes: 100})
	// At the step limit the builder keeps reading, but only to see whether a newer response
	// exists: it asks for more lines until the first one, and then stops.
	if !builder.Add(0, line(1, "abcdef")) || !builder.Add(1, line(2, "xyz")) {
		t.Fatal("Add must go on after the step limit, until it has seen whether the child went on")
	}
	if got := builder.Snapshot(); got == nil || got.MayHaveNewer {
		t.Fatalf("at the limit and with no line after it the last step is the newest: %+v", got)
	}
	if !builder.Add(2, []byte(`{"type":"function_call_output","call_id":"x","output":"late result"}`)) {
		t.Fatal("a line that is no response must not end the look-ahead")
	}
	if builder.Add(3, line(3, "late")) {
		t.Fatal("the first response after the limit must end the look-ahead")
	}
	if builder.Add(4, line(4, "later")) {
		t.Fatal("a finished builder accepted a line")
	}
	snapshot := builder.Snapshot()
	if !snapshot.MayHaveNewer {
		t.Fatalf("a response after the step limit must set MayHaveNewer: %+v", snapshot)
	}
	if len(snapshot.Steps) != 2 || len(snapshot.Steps[0].Parts) != 1 || snapshot.Steps[0].Parts[0].Text != "abc" || snapshot.Steps[0].Parts[0].Truncated == nil || snapshot.Steps[0].Parts[0].Truncated.Bytes != 6 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	budget := NewSubagentBuilder("", SubagentLimits{MaxPartBytes: 100, MaxTotalBytes: 5})
	budget.Add(0, line(1, "abcdefgh"))
	if got := budget.Snapshot(); got == nil || got.MayHaveNewer {
		t.Fatalf("the budget ran out inside the last response, which is the newest: %+v", got)
	}
	budget.Add(1, line(2, "zzz"))
	if got := budget.Snapshot(); got == nil || len(got.Steps) != 1 || got.Steps[0].Parts[0].Text != "abcde" || !got.MayHaveNewer {
		t.Fatalf("total budget: %+v", got)
	}
}

// TestSubagentSnapshotSaysItsLastStepMayNotBeTheNewestResponse: MayHaveNewer is set when a
// response.completed record follows the last step the snapshot shows and the snapshot does
// not show it, and only then; a step added later clears it, a skipped line sets it.
func TestSubagentSnapshotSaysItsLastStepMayNotBeTheNewestResponse(t *testing.T) {
	text := func(step int, text string) []byte {
		return responseLine(step, `{"type":"message","content":[{"type":"output_text","text":"`+text+`"}]}`)
	}
	run := func(limits SubagentLimits, lines ...[]byte) *SubagentSnapshot {
		builder := NewSubagentBuilder("", limits)
		addAll(builder, lines...)
		return builder.Snapshot()
	}
	incomplete := []byte(`{"type":"response.completed","session_turn":1,"session_step":9,"response":{"id":"r9","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"cut off"}]}]}}`)
	unreadable := []byte(`{"type":"response.completed","session_turn":1,"session_step":9,"response":"not an object"}`)
	cases := []struct {
		name   string
		limits SubagentLimits
		lines  [][]byte
		want   bool
	}{
		{"an ordinary child", SubagentLimits{}, [][]byte{text(1, "a"), text(2, "b")}, false},
		{"a tool result after the last step", SubagentLimits{}, [][]byte{responseLine(1, callItem("a")), outputLine("a", "ok")}, false},
		{"the step limit with nothing after it", SubagentLimits{MaxSteps: 2}, [][]byte{text(1, "a"), text(2, "b")}, false},
		{"the step limit with a response after it", SubagentLimits{MaxSteps: 2}, [][]byte{text(1, "a"), text(2, "b"), outputLine("x", "r"), text(3, "c")}, true},
		{"the step limit reached by a result", SubagentLimits{MaxSteps: 1}, [][]byte{responseLine(1, callItem("a")), outputLine("a", "ok"), text(2, "c")}, true},
		{"the step limit reached by the next response", SubagentLimits{MaxSteps: 1}, [][]byte{responseLine(1, callItem("a")), text(2, "c")}, true},
		{"the byte budget with a response after it", SubagentLimits{MaxTotalBytes: 1}, [][]byte{text(1, "abc"), text(2, "c")}, true},
		{"the byte budget with nothing after it", SubagentLimits{MaxTotalBytes: 1}, [][]byte{text(1, "abc")}, false},
		{"a response that is not completed", SubagentLimits{}, [][]byte{text(1, "a"), incomplete}, true},
		{"a response that cannot be read", SubagentLimits{}, [][]byte{text(1, "a"), unreadable}, true},
		{"a response with nothing to keep", SubagentLimits{}, [][]byte{text(1, "a"), responseLine(2, "")}, true},
		{"a refused response and a step after it", SubagentLimits{}, [][]byte{text(1, "a"), incomplete, text(3, "c")}, false},
		{"a step that still waits for a result", SubagentLimits{}, [][]byte{text(1, "a"), responseLine(2, callItem("b"))}, true},
	}
	for _, tc := range cases {
		snapshot := run(tc.limits, tc.lines...)
		if snapshot == nil || snapshot.MayHaveNewer != tc.want {
			t.Errorf("%s: snapshot = %+v, want MayHaveNewer %v", tc.name, snapshot, tc.want)
		}
	}

	// A line the reader skipped may have been a response; one before a later step does not
	// matter, one after the last step does.
	builder := NewSubagentBuilder("", SubagentLimits{})
	addAll(builder, text(1, "a"))
	builder.Unseen()
	if got := builder.Snapshot(); !got.MayHaveNewer {
		t.Fatalf("a skipped line after the last step: %+v", got)
	}
	addAll(builder, text(2, "b"))
	if got := builder.Snapshot(); got.MayHaveNewer {
		t.Fatalf("a step after the skipped line is newer than it: %+v", got)
	}
	// A line skipped between a step's response and its result is newer than that step, which
	// joins the snapshot only when its result arrives.
	skipped := NewSubagentBuilder("", SubagentLimits{})
	addAll(skipped, responseLine(1, callItem("a")))
	skipped.Unseen()
	addAll(skipped, outputLine("a", "ok"))
	if got := skipped.Snapshot(); got == nil || len(got.Steps) != 1 || !got.MayHaveNewer {
		t.Fatalf("a skipped line after a pending step's response: %+v", got)
	}
	// At a limit the look-ahead is over once a line was skipped: it cannot be told apart.
	limited := NewSubagentBuilder("", SubagentLimits{MaxSteps: 1})
	addAll(limited, text(1, "a"))
	limited.Unseen()
	if limited.Add(0, text(2, "b")) || !limited.Snapshot().MayHaveNewer {
		t.Fatalf("a skipped line after the step limit: %+v", limited.Snapshot())
	}

	// It is not part of what a tool result carries of the child.
	snapshot := &SubagentSnapshot{Steps: []SubagentStep{{ID: "1.1", Status: "completed", Parts: []Part{{Type: "text", Text: "x"}}}}, MayHaveNewer: true}
	if summary := snapshot.Summary(); *summary != (SubagentSummary{Steps: 1}) {
		t.Fatalf("summary = %+v", summary)
	}
	encoded, _ := json.Marshal(snapshot)
	if !strings.Contains(string(encoded), `"mayHaveNewer":true`) {
		t.Fatalf("snapshot = %s", encoded)
	}
	snapshot.MayHaveNewer = false
	if encoded, _ = json.Marshal(snapshot); strings.Contains(string(encoded), "mayHaveNewer") {
		t.Fatalf("an ordinary snapshot carries the bit: %s", encoded)
	}
}

// TestSubagentBuilderKeepsAResultThatPrintedNothing pins the wire shape the browser reads: a
// tool that printed nothing still pairs its call, and the JSON part has no output member
// (Output is omitempty), so the client must read a missing output as empty.
func TestSubagentBuilderKeepsAResultThatPrintedNothing(t *testing.T) {
	builder := NewSubagentBuilder("", SubagentLimits{})
	addAll(builder, responseLine(1, callItem("k1")), outputLine("k1", ""))
	snapshot := builder.Snapshot()
	if snapshot == nil || len(snapshot.Steps) != 1 || partTypes(snapshot.Steps[0].Parts) != "tool_call:k1,tool_result:k1" {
		t.Fatalf("snapshot = %+v, want the call paired with its empty result", snapshot)
	}
	encoded, err := json.Marshal(snapshot.Steps[0].Parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"type":"tool_result","callId":"k1","status":"unknown"}` {
		t.Fatalf("empty result JSON = %s", encoded)
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
	// A result carries the summary (agent and step count), never the steps.
	want := SubagentSummary{Agent: "explore", Steps: 1}
	if got := attached[0].Parts[1].Subagent; got == nil || *got != want {
		t.Fatalf("subagent result summary = %+v, want %+v", got, want)
	}
	if got := attached[1].Parts[1].Subagent; got == nil || *got != want {
		t.Fatalf("subagent result summary = %+v, want %+v", got, want)
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

func TestSubagentSummaryIsTheAgentAndTheStepCountOnly(t *testing.T) {
	var none *SubagentSnapshot
	if none.Summary() != nil || (&SubagentSnapshot{Agent: "a"}).Summary() != nil {
		t.Fatal("a missing or empty snapshot has a summary")
	}
	snapshot := &SubagentSnapshot{Agent: "a", Steps: []SubagentStep{
		{ID: "1.1", Status: "completed", Parts: []Part{{Type: "text", Text: "secret step text"}}},
		{ID: "1.2", Status: "completed", Parts: []Part{{Type: "text", Text: "x"}}},
	}}
	encoded, err := json.Marshal(Part{Type: "tool_result", CallID: "c", Output: "ok", Subagent: snapshot.Summary()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"subagent":{"agent":"a","steps":2}`) || strings.Contains(string(encoded), "secret step text") {
		t.Fatalf("encoded part = %s", encoded)
	}
	encoded, _ = json.Marshal(Part{Type: "tool_result", Subagent: (&SubagentSnapshot{Steps: snapshot.Steps[:1]}).Summary()})
	if !strings.Contains(string(encoded), `"subagent":{"steps":1}`) {
		t.Fatalf("unnamed agent: %s", encoded)
	}
}

// handedOffOutput is the result the subagent tool gives a call it hands off: the run id
// and the run's state (queued or running) next to it, and no answer.
func handedOffOutput(state string) string {
	return "{\"id\":\"c1\",\"status\":\"ok\",\"summary\":\"Agent explore is still " + state + " (run sub_0a1b2c3d)\"}\n\n--- INFORMATION ---\nrun: sub_0a1b2c3d\nstate: " + state + "\nelapsed_ms: 12\nlimit_ms: 600000\ntranscript: /store/c1-0a1b2c3d/messages.jsonl\n"
}

// TestSubagentSummarySaysWhetherTheResultOmitsTheLastText: a result omits the child's last
// text unless it came back with it whole: one that names the run's state (handed off), one
// that names truncated (cut), and one with an error code other than subagent_timeout
// (failed, empty, lost) omit it; a finished, stopped or timed-out result does not. A
// "state:" or "truncated:" line in the content is the child's text, not the result's
// information. The flag is on the summary only, set by the replay through the same
// function as the live run, and absent from JSON when false.
func TestSubagentSummarySaysWhetherTheResultOmitsTheLastText(t *testing.T) {
	snapshot := &SubagentSnapshot{Agent: "explore", Steps: []SubagentStep{{ID: "1.1", Status: "completed", Parts: []Part{{Type: "text", Text: "x"}}}}}
	stopped := "{\"id\":\"c1\",\"status\":\"ok\",\"summary\":\"Agent explore was stopped (run sub_0a1b2c3d)\"}\n\n--- INFORMATION ---\nrounds: 1\ntokens: 9\npartial: true\n\n--- CONTENT token=t encoding=utf8 ---\nhalf done\n--- END CONTENT token=t ---\n"
	errorOutput := func(code, extra string) string {
		return "{\"id\":\"c1\",\"status\":\"error\",\"summary\":\"Agent explore did not end well\"}\n\n--- INFORMATION ---\nerror_type: execution\nerror_code: " + code + "\nagent: explore\n" + extra + "\n--- CONTENT token=t encoding=utf8 ---\nhalf done\n--- END CONTENT token=t ---\n"
	}
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"finished", subagentOutput(""), false},
		{"finished and cut", subagentOutput("truncated: true\n"), true},
		{"running", handedOffOutput("running"), true},
		{"queued", handedOffOutput("queued"), true},
		{"stopped", stopped, false},
		{"stopped and cut", strings.Replace(stopped, "partial: true\n", "partial: true\ntruncated: true\n", 1), true},
		{"timed out", errorOutput("subagent_timeout", "partial: true\n"), false},
		{"timed out and cut", errorOutput("subagent_timeout", "partial: true\ntruncated: true\n"), true},
		{"lost", errorOutput("subagent_lost", "partial: true\n"), true},
		{"failed", errorOutput("subagent_failed", ""), true},
		{"empty", errorOutput("subagent_empty_result", ""), true},
		{"state in content", strings.Replace(subagentOutput(""), "the answer", "state: running", 1), false},
		{"truncated in content", strings.Replace(subagentOutput(""), "the answer", "truncated: true", 1), false},
		{"no information", "plain output", false},
	}
	for _, tc := range cases {
		message := toolMessage("subagent", `{"agent":"explore"}`, tc.output)
		call, ok := SubagentCallOf(message.Parts[0], message.Parts[1])
		if !ok || call.OmitsLastText != tc.want {
			t.Errorf("%s: call = %+v, %v; want omits last text %v", tc.name, call, ok, tc.want)
		}
		attached := AttachSubagents([]Message{message}, func(SubagentCall) *SubagentSnapshot { return snapshot })
		want := SubagentSummary{Agent: "explore", Steps: 1, OmitsLastText: tc.want}
		if got := attached[0].Parts[1].Subagent; got == nil || *got != want {
			t.Errorf("%s: replay summary = %+v, want %+v", tc.name, got, want)
		}
	}
	if SubagentSummaryOf(SubagentCall{OmitsLastText: true}, nil) != nil || SubagentSummaryOf(SubagentCall{OmitsLastText: true}, &SubagentSnapshot{}) != nil {
		t.Fatal("a child with no step has a summary")
	}
	encoded, _ := json.Marshal(Part{Type: "tool_result", Subagent: SubagentSummaryOf(SubagentCall{OmitsLastText: true}, snapshot)})
	if !strings.Contains(string(encoded), `"subagent":{"agent":"explore","steps":1,"omitsLastText":true}`) {
		t.Fatalf("summary JSON of a result that omits the last text = %s", encoded)
	}
	encoded, _ = json.Marshal(Part{Type: "tool_result", Subagent: SubagentSummaryOf(SubagentCall{}, snapshot)})
	if strings.Contains(string(encoded), "omitsLastText") {
		t.Fatalf("a finished result's summary names omitsLastText: %s", encoded)
	}
}

func TestCapMessagesLeavesTheSubagentSummaryAlone(t *testing.T) {
	summary := &SubagentSummary{Agent: "a", Steps: 3}
	messages := []Message{{ID: "m", Sender: "assistant", Parts: []Part{{Type: "tool_result", CallID: "c", Output: "ok", Subagent: summary}}}}
	for _, limit := range []int{0, 1, 1 << 20} {
		if got := CapMessages(messages, limit, nil)[0].Parts[0].Subagent; got != summary {
			t.Fatalf("limit %d replaced the summary: %+v", limit, got)
		}
	}
}

func TestIncrementsReportsAChangedSubagentSummary(t *testing.T) {
	part := Part{Type: "tool_result", CallID: "c", Output: "ok"}
	with := part
	with.Subagent = &SubagentSummary{Steps: 1}
	events := Increments(Message{ID: "m", Parts: []Part{part}}, Message{ID: "m", Parts: []Part{with}})
	if len(events) != 1 || events[0].Name != "ui.part" {
		t.Fatalf("events = %+v, want one ui.part for the part that gained a summary", events)
	}
	if events := Increments(Message{ID: "m", Parts: []Part{with}}, Message{ID: "m", Parts: []Part{with}}); len(events) != 0 {
		t.Fatalf("an unchanged summary produced %+v", events)
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
