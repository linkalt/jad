package app

import (
	"context"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// R5c review: a stop of a message is chosen while the message is live and can land after its
// target run ended, after attach's watch already looked for a stop; and a message can end while
// its start is on its way, so the started run is attached to a finished message. In both cases
// what the target run started must still stop. Both gaps are entered deterministically here.

// prepareLineageMessage prepares dev/coder's message to review/reviewer, whose run starts finder
// ("look around") as a handle and ends (newLineageChain).
func prepareLineageMessage(t *testing.T, c *lineageChain) (*profileMessagePlan, context.Context) {
	t.Helper()
	reference, err := c.f.service.resolver.CanonicalSessionReference("local:demo")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), callerMessagesPathKey{}, boundRunnerOf(t, c.f, "dev/coder").config.MessagesPath)
	request := native.ProfileMessageRequest{OwnerKey: sessionKey(reference, "dev/coder"), Target: "review/reviewer", Message: "please review", MaxQueued: 4, Timeout: time.Minute}
	plan, err := profileMessenger{resolver: c.f.service.resolver}.Prepare(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return plan.(*profileMessagePlan), ctx
}

// A message that ended on its own leaves finder going; a stop chosen while it was live that lands
// only now still stops finder, once, and leaves the reviewer's own work alone.
func TestLineageAStopThatLandsAfterTheMessageEndedReachesItsWork(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer"})
	c.startOwnWork(t, "demo")
	plan, ctx := prepareLineageMessage(t, c)
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, c.finder, "the chain's finder request")
	c.finder <- struct{}{}
	awaitSignal(t, run.Done(), "the message to end on its own")
	if outcome := run.Outcome(); outcome.Reason != "" {
		t.Fatalf("the message ended with %+v, want no stop", outcome)
	}
	c.expect(t, "look around", "running", "")
	run.Stop("user_stop")
	c.expect(t, "look around", "stopped", "user_stop")
	c.expect(t, "watch the logs", "running", "")
	if outcome := run.Outcome(); outcome.Reason != "" || outcome.Err == native.ErrProfileStopped {
		t.Fatalf("the late stop changed the message's outcome: %+v", outcome)
	}
}

// A message stopped before its started run was attached (a queued message stopped while the
// line's pump started it) stops the run at once; what the run started meanwhile stops too.
func TestLineageARunAttachedToAFinishedMessageTakesItsWorkAlong(t *testing.T) {
	c := newLineageChain(t, lineageOptions{target: "review/reviewer"})
	plan, ctx := prepareLineageMessage(t, c)
	message := newProfileMessageRun(plan)
	target, err := plan.startRun(ctx, message, false)
	if err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, c.finder, "the chain's finder request")
	c.finder <- struct{}{}
	c.expect(t, "look around", "running", "")
	// The line's stop handler: the stop lands before the run is attached, and finishes the message.
	message.Stop("user_stop")
	message.finish(native.ProfileMessageOutcome{Err: message.endedError(), Reason: message.reason()})
	message.attach(target)
	awaitSignal(t, target.Done(), "the late run to stop")
	c.expect(t, "look around", "stopped", "user_stop")
}
