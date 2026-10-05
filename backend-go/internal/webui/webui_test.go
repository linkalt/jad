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
