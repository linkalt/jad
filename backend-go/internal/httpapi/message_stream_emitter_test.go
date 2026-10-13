package httpapi

import (
	"testing"
	"time"

	"jad/backend-go/internal/webui"
)

func TestMessageStreamEmitterCoalescesTextAndFlushesStructuralChange(t *testing.T) {
	var published []webui.Message
	emitter := newMessageStreamEmitter(time.Hour, func(message webui.Message) error {
		published = append(published, cloneStreamMessage(message))
		return nil
	})
	defer emitter.discard()

	for _, message := range []webui.Message{
		streamTextMessage("a"),
		streamTextMessage("ab"),
		streamTextMessage("abc"),
	} {
		if err := emitter.push(message); err != nil {
			t.Fatalf("push(%q) error = %v", message.Text, err)
		}
	}
	if len(published) != 1 {
		t.Fatalf("published before flush = %d, want 1", len(published))
	}
	if err := emitter.flush(); err != nil {
		t.Fatalf("flush() error = %v", err)
	}
	if len(published) != 2 || published[1].Parts[0].Text != "abc" {
		t.Fatalf("published after flush = %+v, want cumulative text abc", published)
	}

	complete := streamTextMessage("abc")
	complete.Status = "complete"
	if err := emitter.push(complete); err != nil {
		t.Fatalf("push(structural change) error = %v", err)
	}
	if len(published) != 3 || published[2].Status != "complete" {
		t.Fatalf("published structural update = %+v", published)
	}
}

func TestMessageStreamEmitterCoalescesToolArguments(t *testing.T) {
	var published []webui.Message
	emitter := newMessageStreamEmitter(time.Hour, func(message webui.Message) error {
		published = append(published, cloneStreamMessage(message))
		return nil
	})
	defer emitter.discard()

	argument := "{"
	initial := webui.Message{
		ID:     "turn",
		Status: "streaming",
		Parts:  []webui.Part{{Type: "tool_call", CallID: "call-1", Name: "search", Arguments: &argument, Partial: true}},
	}
	if err := emitter.push(initial); err != nil {
		t.Fatalf("push(initial) error = %v", err)
	}
	argument = `{"query"`
	second := cloneStreamMessage(initial)
	second.Parts[0].Arguments = &argument
	if err := emitter.push(second); err != nil {
		t.Fatalf("push(second) error = %v", err)
	}
	argument = `{"query":"x"}`
	third := cloneStreamMessage(initial)
	third.Parts[0].Arguments = &argument
	if err := emitter.push(third); err != nil {
		t.Fatalf("push(third) error = %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published before flush = %d, want 1", len(published))
	}
	if err := emitter.flush(); err != nil {
		t.Fatalf("flush() error = %v", err)
	}
	if len(published) != 2 || published[1].Parts[0].Arguments == nil || *published[1].Parts[0].Arguments != `{"query":"x"}` {
		t.Fatalf("published tool arguments = %+v", published)
	}
}

func TestMessageStreamEmitterFlushesAfterIntervalAndCanBeDisabled(t *testing.T) {
	emitted := make(chan webui.Message, 4)
	emitter := newMessageStreamEmitter(20*time.Millisecond, func(message webui.Message) error {
		emitted <- cloneStreamMessage(message)
		return nil
	})
	if err := emitter.push(streamTextMessage("a")); err != nil {
		t.Fatalf("push(initial) error = %v", err)
	}
	if got := <-emitted; got.Text != "a" {
		t.Fatalf("initial message = %q", got.Text)
	}
	if err := emitter.push(streamTextMessage("ab")); err != nil {
		t.Fatalf("push(appended) error = %v", err)
	}
	select {
	case got := <-emitted:
		if got.Text != "ab" {
			t.Fatalf("timer-flushed message = %q, want ab", got.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timer did not flush pending message")
	}
	emitter.discard()

	var immediate []webui.Message
	disabled := newMessageStreamEmitter(0, func(message webui.Message) error {
		immediate = append(immediate, cloneStreamMessage(message))
		return nil
	})
	defer disabled.discard()
	if err := disabled.push(streamTextMessage("x")); err != nil {
		t.Fatalf("disabled push(initial) error = %v", err)
	}
	if err := disabled.push(streamTextMessage("xy")); err != nil {
		t.Fatalf("disabled push(appended) error = %v", err)
	}
	if len(immediate) != 2 {
		t.Fatalf("disabled interval emitted %d snapshots, want 2", len(immediate))
	}
}

func TestMessageStreamEmitterDiscardDropsPendingSnapshot(t *testing.T) {
	var published []webui.Message
	emitter := newMessageStreamEmitter(time.Hour, func(message webui.Message) error {
		published = append(published, cloneStreamMessage(message))
		return nil
	})
	if err := emitter.push(streamTextMessage("a")); err != nil {
		t.Fatal(err)
	}
	if err := emitter.push(streamTextMessage("ab")); err != nil {
		t.Fatal(err)
	}
	emitter.discard()
	if len(published) != 1 {
		t.Fatalf("discarded pending snapshot was published: %d messages", len(published))
	}
}

func streamTextMessage(text string) webui.Message {
	return webui.Message{
		ID:     "turn",
		Status: "streaming",
		Text:   text,
		Parts:  []webui.Part{{Type: "text", Text: text}},
	}
}
