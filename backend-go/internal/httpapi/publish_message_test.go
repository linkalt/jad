package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/webui"
)

func drainFrames(t *testing.T, queue *eventQueue, want int) []string {
	t.Helper()
	var frames []string
	for len(frames) < want {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		event, ok := queue.next(ctx)
		cancel()
		if !ok {
			t.Fatalf("queue ended after %d frames, want %d", len(frames), want)
		}
		frames = append(frames, string(event.frame))
	}
	return frames
}

// publishMessage must send the same frames as before the encoding moved out of the
// lock: the whole message first, then increments that take a client from one message
// to the next.
func TestPublishMessageSendsWholeMessageThenIncrements(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<20)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	queue := run.subscribe()
	first := webUIMessage{ID: "m1", Sender: "assistant", Status: "streaming", Parts: []webui.Part{{Type: "text", Text: "hel"}}}
	second := first
	second.Parts = []webui.Part{{Type: "text", Text: "hello"}}
	if err := run.publishMessage(first); err != nil {
		t.Fatal(err)
	}
	if err := run.publishMessage(second); err != nil {
		t.Fatal(err)
	}
	frames := drainFrames(t, queue, 3)
	if !strings.HasPrefix(frames[0], "event: run\n") {
		t.Fatalf("first frame = %q, want the run summary", frames[0])
	}
	if !strings.HasPrefix(frames[1], "event: ui.message\n") || !strings.Contains(frames[1], `"hel"`) {
		t.Fatalf("second frame = %q, want the whole first message", frames[1])
	}
	if !strings.Contains(frames[2], "ui.delta") || !strings.Contains(frames[2], `"lo"`) {
		t.Fatalf("third frame = %q, want a delta carrying the appended text", frames[2])
	}
	if current := run.currentMessage(); current == nil || current.Parts[0].Text != "hello" {
		t.Fatalf("currentMessage() = %+v, want the second message", current)
	}
	late := run.subscribe()
	lateFrames := drainFrames(t, late, 2)
	if !strings.Contains(lateFrames[1], `"hello"`) {
		t.Fatalf("late subscriber got %q, want the latest whole message", lateFrames[1])
	}
}

// Publishers racing on one run must leave a consistent chain: whatever order they win
// the lock in, the frames a subscriber receives, applied in order, rebuild the message
// the run holds as current. Each writer sends text that grows, but the writers do not
// agree on the text, so a delta built against a stale previous message would show up
// as a client that holds something the run does not.
func TestPublishMessageConcurrentPublishersKeepAConsistentChain(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<26)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	queue := run.subscribe()
	var group sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for revision := 1; revision <= 60; revision++ {
				message := webUIMessage{ID: "m", Sender: "assistant", Status: "streaming", Parts: []webui.Part{{Type: "text", Text: strings.Repeat(string(rune('a'+writer)), revision)}}}
				_ = run.publishMessage(message)
			}
		}()
	}
	group.Wait()
	current := run.currentMessage()
	if current == nil || len(current.Parts) != 1 {
		t.Fatalf("currentMessage() = %+v", current)
	}
	want := current.Parts[0].Text

	held := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		event, ok := queue.next(ctx)
		cancel()
		if !ok {
			break
		}
		frame := string(event.frame)
		switch {
		case strings.HasPrefix(frame, "event: ui.message\n"):
			held = textOfFrame(t, frame, `"text":"`)
		case strings.HasPrefix(frame, "event: ui.part\n"):
			held = textOfFrame(t, frame, `"text":"`)
		case strings.HasPrefix(frame, "event: ui.delta\n"):
			held += textOfFrame(t, frame, `"append":"`)
		}
	}
	if held != want {
		t.Fatalf("a client replaying the frames holds %q (%d bytes), the run holds %q (%d bytes)", abbreviate(held), len(held), abbreviate(want), len(want))
	}
}

func textOfFrame(t *testing.T, frame, key string) string {
	t.Helper()
	data := frame[strings.Index(frame, "data: ")+len("data: "):]
	at := strings.LastIndex(data, key)
	if at < 0 {
		return ""
	}
	rest := data[at+len(key):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("unterminated value in %q", frame)
	}
	return rest[:end]
}

func abbreviate(text string) string {
	if len(text) > 20 {
		return text[:20] + "..."
	}
	return text
}
