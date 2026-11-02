package app

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// A queued message that is stopped while it waits for its target must end, close its done
// channel, and record its outcome once, without its target ever having started.
func TestProfileMessageStopOfAWaitingQueuedMessageEndsItAndFinishesIt(t *testing.T) {
	p := newPMFixture(t, nil)
	release := p.holdTranscript(t)
	defer release()
	request := native.ProfileMessageRequest{OwnerKey: p.owner, Target: "review/reviewer", Message: "alpha-1", MaxQueued: 4}
	plan, err := profileMessenger{resolver: p.f.service.resolver}.Prepare(p.callerCtx(t, "dev/coder"), request)
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(p.callerCtx(t, "dev/coder"))
	if err != nil {
		t.Fatal(err)
	}
	if !run.Queued() {
		t.Fatal("the message must queue behind the held transcript")
	}
	run.Stop("caller_cancelled")
	awaitSignal(t, run.Done(), "the stopped queued message to finish")
	if outcome := run.Outcome(); outcome.Err != native.ErrProfileStopped || outcome.Reason != "caller_cancelled" {
		t.Fatalf("outcome = %+v, want stopped with caller_cancelled", outcome)
	}
	if position, length := p.f.service.resolver.profileQueues.position(plan.(*profileMessagePlan).key, run.(*profileMessageRun)); position != 0 || length != 0 {
		t.Fatalf("the stopped message is still in the line at %d of %d", position, length)
	}
}

// Queued messages that finish normally must not leave a goroutine behind once they are done.
func TestProfileMessageQueuedCompletionLeavesNoGoroutines(t *testing.T) {
	p := newPMFixture(t, nil)
	cycle := func() {
		release := p.holdTranscript(t)
		queued := p.handle(t, p.send(t, "alpha-1"))
		release()
		if got := p.wait(t, queued); !strings.Contains(got, `"state":"done"`) {
			t.Fatalf("the queued message did not complete:\n%s", got)
		}
		awaitTargetSessionFree(t, p.f)
	}
	cycle()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 10 {
		cycle()
	}
	http.DefaultClient.CloseIdleConnections()
	if after := settledGoroutineCount(before); after > before {
		t.Fatalf("goroutines %d -> %d after 10 queued completions", before, after)
	}
}

// A queued message that is not at the head of its target's line has no pump blocked on it,
// so a stop must finish it on its own while the message ahead of it still waits.
func TestProfileMessageStopOfAQueuedMessageBehindTheHeadFinishesItWhileTheHeadWaits(t *testing.T) {
	p := newPMFixture(t, nil)
	release := p.holdTranscript(t)
	defer release()
	head := p.handle(t, p.send(t, "alpha-1"))
	request := native.ProfileMessageRequest{OwnerKey: p.owner, Target: "review/reviewer", Message: "alpha-2", MaxQueued: 4}
	plan, err := profileMessenger{resolver: p.f.service.resolver}.Prepare(p.callerCtx(t, "dev/coder"), request)
	if err != nil {
		t.Fatal(err)
	}
	behind, err := plan.Start(p.callerCtx(t, "dev/coder"))
	if err != nil {
		t.Fatal(err)
	}
	if position, _ := p.f.service.resolver.profileQueues.position(plan.(*profileMessagePlan).key, behind.(*profileMessageRun)); position != 2 {
		t.Fatalf("the message must wait behind the head, at place %d", position)
	}
	behind.Stop("caller_cancelled")
	awaitSignal(t, behind.Done(), "the stopped message behind the head to finish")
	if outcome := behind.Outcome(); outcome.Err != native.ErrProfileStopped {
		t.Fatalf("outcome = %+v, want stopped", outcome)
	}
	if got := p.asyncAs(t, p.owner, map[string]any{"action": "status", "id": head}); !strings.Contains(got, `"state":"queued"`) {
		t.Fatalf("the head must still be waiting for the transcript:\n%s", got)
	}
}

// A queued message whose queue wait ends at once must finish and leave its stop watch
// unregistered, whichever of beginQueued and the expiry runs first.
func TestProfileMessageQueueWaitEndingAtOnceFinishesTheMessage(t *testing.T) {
	p := newPMFixture(t, func(s *sessionfs.NativeProfileMessageSettings) { s.BusyWaitMs = 1 })
	release := p.holdTranscript(t)
	defer release()
	id := p.handle(t, p.send(t, "alpha-1"))
	if got := p.wait(t, id); !strings.Contains(got, "profile_busy") {
		t.Fatalf("the message must expire with profile_busy:\n%s", got)
	}
	if served := p.order.served(); len(served) != 0 {
		t.Fatalf("the expired message ran: %v", served)
	}
}
