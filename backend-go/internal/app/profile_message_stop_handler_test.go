package app

import (
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// R5 pre-change characterisation of the message's own stop handlers, apart from the call side.
// The mixed-chain stop test reaches the target through the waiting call's
// run.Stop("caller_cancelled") (profileMessageTool.execute). These tests stop the message run
// itself, with no caller context ending, so they pin the handlers profileMessageRun has for that
// (attach's watch, and onCtxDone for a message that queued first) before persistent starts are
// routed through StartRun.

// stopHandlerFixture is a pm fixture whose target run holds its first provider request until the
// test ends; reached signals that the target run is working.
func stopHandlerFixture(t *testing.T) (*pmFixture, <-chan struct{}) {
	t.Helper()
	p := newPMFixture(t, nil)
	reached := make(chan struct{}, 4)
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	p.f.script = func(string, string) string {
		reached <- struct{}{}
		select {
		case <-released:
		case <-time.After(20 * time.Second):
		}
		return ""
	}
	return p, reached
}

func startStopHandlerMessage(t *testing.T, p *pmFixture) (*profileMessagePlan, native.ProfileMessageRun) {
	t.Helper()
	request := native.ProfileMessageRequest{OwnerKey: p.owner, Target: "review/reviewer", Message: "alpha-1", MaxQueued: 4, Timeout: time.Minute}
	plan, err := profileMessenger{resolver: p.f.service.resolver}.Prepare(p.callerCtx(t, "dev/coder"), request)
	if err != nil {
		t.Fatal(err)
	}
	run, err := plan.Start(p.callerCtx(t, "dev/coder"))
	if err != nil {
		t.Fatal(err)
	}
	return plan.(*profileMessagePlan), run
}

func requireStoppedMessage(t *testing.T, p *pmFixture, run native.ProfileMessageRun, reason string) {
	t.Helper()
	awaitSignal(t, run.Done(), "the stopped message to finish")
	outcome := run.Outcome()
	if outcome.Err != native.ErrProfileStopped || outcome.Reason != reason {
		t.Fatalf("outcome = %+v, want stopped with %s", outcome, reason)
	}
	pm := run.(*profileMessageRun)
	pm.mu.Lock()
	target := pm.run
	pm.mu.Unlock()
	if target == nil || target.Status() != "aborted" {
		t.Fatalf("the target run was not stopped: %v", target)
	}
	awaitTargetSessionFree(t, p.f)
}

// A message that started at once is stopped by Stop alone: attach's watch sees the message's
// context end and stops the target run.
func TestProfileMessageStopOfAStartedMessageStopsTheTargetRunWithoutTheCaller(t *testing.T) {
	p, reached := stopHandlerFixture(t)
	_, run := startStopHandlerMessage(t, p)
	if run.Queued() {
		t.Fatal("the message must start at once on a free target")
	}
	awaitSignal(t, reached, "the target run to send its request")
	run.Stop("owner_stop")
	requireStoppedMessage(t, p, run, "owner_stop")
}

// A message that queued and then started is stopped by Stop alone as well: it is watched by
// attach and by the queue's onCtxDone, and either one stops the target run.
func TestProfileMessageStopOfAQueuedThenStartedMessageStopsTheTargetRun(t *testing.T) {
	p, reached := stopHandlerFixture(t)
	release := p.holdTranscript(t)
	_, run := startStopHandlerMessage(t, p)
	if !run.Queued() {
		release()
		t.Fatal("the message must queue behind the held transcript")
	}
	release()
	awaitSignal(t, run.Started(), "the queued message to start")
	awaitSignal(t, reached, "the target run to send its request")
	run.Stop("owner_stop")
	requireStoppedMessage(t, p, run, "owner_stop")
}
