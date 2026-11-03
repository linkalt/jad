package httpapi

import (
	"context"
	"testing"
)

// takeQueued hands the SSE writer every frame queued behind the one it took, in order, and
// gives their bytes back to the budget, so the next frames are not refused (audit P1).
func TestTakeQueuedReturnsTheBacklogInOrderAndFreesTheBudget(t *testing.T) {
	queue := newEventQueue(30)
	for _, name := range []string{"a", "b", "c"} {
		if !queue.push(streamEvent{name: name, frame: []byte("0123456789")}, false) {
			t.Fatalf("push %s refused", name)
		}
	}
	first, ok := queue.next(context.Background())
	if !ok || first.name != "a" {
		t.Fatalf("next = %q, %v", first.name, ok)
	}
	rest := queue.takeQueued()
	if len(rest) != 2 || rest[0].name != "b" || rest[1].name != "c" {
		t.Fatalf("takeQueued = %v, want b then c", rest)
	}
	if more := queue.takeQueued(); len(more) != 0 {
		t.Fatalf("a second takeQueued returned %d frames", len(more))
	}
	for _, name := range []string{"d", "e", "f"} {
		if !queue.push(streamEvent{name: name, frame: []byte("0123456789")}, false) {
			t.Fatalf("push %s refused after the backlog was taken: the budget was not freed", name)
		}
	}
}

func TestTakeQueuedAfterAnOversizedSnapshotKeepsTheBudgetConsistent(t *testing.T) {
	queue := newEventQueue(10)
	if !queue.push(streamEvent{name: "snapshot", frame: make([]byte, 50)}, true) {
		t.Fatal("oversized snapshot refused")
	}
	if got := queue.takeQueued(); len(got) != 1 || got[0].name != "snapshot" {
		t.Fatalf("takeQueued = %v", got)
	}
	if !queue.push(streamEvent{name: "delta", frame: make([]byte, 10)}, false) {
		t.Fatal("a frame that fits the budget was refused after the snapshot was taken")
	}
}
