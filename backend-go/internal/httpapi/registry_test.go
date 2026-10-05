package httpapi

import (
	"context"
	"testing"
)

func TestEventQueueResynchronizesWithinBufferedByteLimit(t *testing.T) {
	queue := newEventQueue(96)
	runEvent, err := makeSSEEvent("run", map[string]string{"status": "running"})
	if err != nil {
		t.Fatal(err)
	}
	if !queue.push(runEvent, false) {
		t.Fatal("could not enqueue run summary")
	}

	message := webUIMessage{
		ID:        "turn-1",
		Sender:    "assistant",
		Timestamp: "1:00 PM",
		Parts:     textParts("snapshot text that is long enough to replace the queued events"),
		Status:    "streaming",
		Text:      "snapshot text that is long enough to replace the queued events",
	}
	snapshotEvent, err := makeSSEEvent("ui.message", map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	if !queue.push(snapshotEvent, true) {
		t.Fatal("snapshot should replace older queued events")
	}
	if len(queue.queue) != 1 || queue.queue[0].name != "ui.message" {
		t.Fatalf("queued events after resync = %#v, want only ui.message", queue.queue)
	}

	completedEvent, err := makeSSEEvent("completed", map[string]any{"message": message, "committed": true})
	if err != nil {
		t.Fatal(err)
	}
	if !queue.push(completedEvent, true) {
		t.Fatal("completed snapshot should replace an over-limit queue")
	}
	doneEvent, err := makeSSEEvent("done", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !queue.push(doneEvent, false) {
		t.Fatal("done should follow a terminal snapshot")
	}
	if len(queue.queue) != 2 || queue.queue[0].name != "completed" || queue.queue[1].name != "done" {
		t.Fatalf("terminal queue = %#v, want completed then done", queue.queue)
	}

	first, ok := queue.next(context.Background())
	if !ok || first.name != "completed" {
		t.Fatalf("first replay event = %#v, ok=%v", first, ok)
	}
	if events := parseEvents(t, string(first.frame)); len(events) != 1 || events[0].name != "completed" {
		t.Fatalf("first replay frame = %#v", events)
	}
	second, ok := queue.next(context.Background())
	if !ok || second.name != "done" {
		t.Fatalf("second replay event = %#v, ok=%v", second, ok)
	}
}
