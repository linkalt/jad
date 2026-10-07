package httpapi

import (
	"context"
	"strings"
	"testing"
)

// bigSnapshot is a full message snapshot far larger than the small budgets the
// queue tests use.
func bigSnapshot(t *testing.T, name string) streamEvent {
	t.Helper()
	message := webUIMessage{
		ID: "turn-1", Sender: "assistant", Timestamp: "1:00 PM", Status: "streaming",
		Parts: textParts(strings.Repeat("x", 400)), Text: strings.Repeat("x", 400),
	}
	event, err := makeSSEEvent(name, map[string]any{"message": message, "committed": true})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func smallEvent(t *testing.T, name string) streamEvent {
	t.Helper()
	event, err := makeSSEEvent(name, map[string]string{"append": strings.Repeat("y", 20)})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestEventQueueKeepsTheStreamAfterAnOversizedSnapshotIsRead(t *testing.T) {
	queue := newEventQueue(96)
	if !queue.push(bigSnapshot(t, "ui.message"), true) {
		t.Fatal("an oversized snapshot is the smallest unit and must be accepted")
	}
	first, ok := queue.next(context.Background())
	if !ok || first.name != "ui.message" {
		t.Fatalf("first event = %#v, ok=%v", first, ok)
	}
	// The client has read it, so the next event must not close the queue.
	if !queue.push(smallEvent(t, "ui.delta"), false) {
		t.Fatal("a small event after a read oversized snapshot closed the queue")
	}
	if next, ok := queue.next(context.Background()); !ok || next.name != "ui.delta" {
		t.Fatalf("next event = %#v, ok=%v", next, ok)
	}
}

func TestEventQueueDoesNotChargeAnOversizedSnapshotToTheEventsBehindIt(t *testing.T) {
	queue := newEventQueue(96)
	queue.push(bigSnapshot(t, "ui.message"), true)
	// Two small events fit the budget (each frame is about 50 bytes); a third does
	// not, so a stalled client is still bounded.
	if !queue.push(smallEvent(t, "ui.delta"), false) {
		t.Fatal("first event behind the snapshot was refused")
	}
	if queue.push(smallEvent(t, "ui.delta"), false) && queue.push(smallEvent(t, "ui.delta"), false) {
		t.Fatal("events behind the snapshot are not bounded by the budget")
	}
}

func TestEventQueueAlwaysQueuesTheTerminalEvents(t *testing.T) {
	queue := newEventQueue(96)
	queue.push(bigSnapshot(t, "ui.message"), true)
	// The closing burst arrives in one go, before the client reads anything.
	if !queue.push(bigSnapshot(t, "completed"), true) {
		t.Fatal("completed was refused behind an oversized snapshot")
	}
	if !queue.push(smallEvent(t, "done"), false) {
		t.Fatal("done was refused")
	}
	var names []string
	for len(queue.queue) > 0 {
		event, _ := queue.next(context.Background())
		names = append(names, event.name)
	}
	if strings.Join(names, ",") != "completed,done" {
		t.Fatalf("terminal queue = %v, want completed,done", names)
	}

	failed := newEventQueue(96)
	// One event of about 50 bytes; a second would not fit the budget.
	if !failed.push(smallEvent(t, "ui.delta"), false) {
		t.Fatal("could not fill the budget")
	}
	if !failed.push(bigSnapshot(t, "error"), false) || !failed.push(smallEvent(t, "done"), false) {
		t.Fatal("error and done must be queued even when the budget is spent")
	}
}

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
