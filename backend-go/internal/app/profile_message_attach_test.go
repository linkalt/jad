package app

import (
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// A non-queued message that is stopped while its target run is working must still end with
// its outcome recorded, once, and with the target run cancelled.
func TestProfileMessageCancelledNonQueuedStillFinishesAndConcludesOnce(t *testing.T) {
	f := newLayoutFixture(t)
	f.cancelled = make(chan struct{}, 4)
	f.mu.Lock()
	f.hold, f.reached = true, make(chan struct{}, 4)
	f.mu.Unlock()
	messenger := profileMessenger{resolver: f.service.resolver}
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi"}
	plan, err := messenger.Prepare(callerContext(t, f, context.Background()), request)
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(callerContext(t, f, context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, f.reached, "the target's provider request")
	run.Stop("caller_cancelled")
	awaitSignal(t, run.Done(), "the message to finish")
	awaitSignal(t, f.cancelled, "the target's request to be cancelled")
	first := run.Outcome()
	if first.Err != native.ErrProfileStopped || first.Reason != "caller_cancelled" {
		t.Fatalf("outcome = %+v, want stopped with caller_cancelled", first)
	}
	if again := run.Outcome(); again != first {
		t.Fatalf("outcome changed after it was recorded: %+v then %+v", first, again)
	}
	awaitTargetSessionFree(t, f)
}

// A non-queued message that finishes must not leave a goroutine behind once it is done.
func TestProfileMessageNonQueuedLeavesNoGoroutinesAfterCompletion(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	send := func() {
		if _, err := messenger.Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi", 0, false); err != nil {
			t.Fatal(err)
		}
	}
	send()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 10 {
		send()
	}
	http.DefaultClient.CloseIdleConnections()
	if after := settledGoroutineCount(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after 10 non-queued messages\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
}
