package httpapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"jad/backend-go/internal/webui"
)

// A coalesced snapshot must stay what the run produced when the run keeps streaming and
// when a published message is changed by its receiver.
func TestMessageStreamEmitterSnapshotsStayIndependentOfTheRun(t *testing.T) {
	var published []webui.Message
	emitter := newMessageStreamEmitter(time.Hour, func(message webui.Message) error {
		published = append(published, message)
		return nil
	})
	defer emitter.discard()
	run := webui.NewStreamingRun("turn", "now")
	if _, ok := run.AddToolResult(json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"one"}`)); !ok {
		t.Fatal("tool result was not added")
	}
	events := []string{
		`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"a"}`,
		`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"b"}`,
		`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"c"}`,
	}
	var last webui.Message
	for _, event := range events {
		message, changed := run.Update(json.RawMessage(event))
		if !changed {
			t.Fatal("delta did not change the message")
		}
		if err := emitter.push(message); err != nil {
			t.Fatal(err)
		}
		last = message
	}
	want := run.Message("streaming")
	if len(published) != 1 {
		t.Fatalf("published before flush = %d, want 1", len(published))
	}
	published[0].Parts[len(published[0].Parts)-1].Text = "changed by the receiver"
	if err := emitter.flush(); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || !reflect.DeepEqual(published[1], want) {
		t.Fatalf("flushed snapshot = %+v, want %+v", published[1], want)
	}
	// Later run updates and receiver edits of the flushed message leave the next diff intact.
	published[1].Parts[0].Output = "edited"
	more, _ := run.Update(json.RawMessage(`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"d"}`))
	if reflect.DeepEqual(more.Parts, last.Parts) {
		t.Fatal("run update did not change the new snapshot")
	}
	if last.Parts[len(last.Parts)-1].Text != "abc" {
		t.Fatalf("earlier snapshot changed to %q after a later update", last.Parts[len(last.Parts)-1].Text)
	}
}

func TestStreamingRunMessageDoesNotAliasRunParts(t *testing.T) {
	run := webui.NewStreamingRun("turn", "now")
	first, _ := run.Update(json.RawMessage(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"n","arguments":"{"}}`))
	if len(first.Parts) == 0 || first.Parts[0].Arguments == nil {
		t.Fatalf("no tool call part: %+v", first)
	}
	*first.Parts[0].Arguments = "mutated"
	first.Parts[0].Text = "mutated"
	again := run.Message("streaming")
	if again.Parts[0].Arguments == nil || *again.Parts[0].Arguments == "mutated" || again.Parts[0].Text == "mutated" {
		t.Fatalf("snapshot shares storage with the run: %+v", again.Parts[0])
	}
}
