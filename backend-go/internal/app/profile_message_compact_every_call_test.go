package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// A compact context_trigger is evaluated on every message that starts a run of the target, not
// once per caller run. A reset is too (profile_message_reset_every_call_test.go).

// TestCompactTriggerEvaluatesEveryCallOfOneCallerRun: several messages of one caller run to one
// target compact each time at after 0, and so does a reset in the same sequence.
func TestCompactTriggerEvaluatesEveryCallOfOneCallerRun(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   int
	}{{sessionfs.ProfileMessageContextCompact, 3}, {sessionfs.ProfileMessageContextReset, 3}} {
		t.Run(tc.action, func(t *testing.T) {
			f := newLayoutFixture(t)
			writeTargetTranscript(t, f, userLine)
			failOnUsageRead(t, f)
			compactions, resets := countCompactions(f), countResets(f)
			for i := 0; i < 3; i++ {
				sendWithTrigger(t, f, 1, "hi", contextTrigger(tc.action, 0))
			}
			if got := len(*compactions) + len(*resets); got != tc.want {
				t.Fatalf("%s acted %d times (compactions %v, resets %v), want %d", tc.action, got, *compactions, *resets, tc.want)
			}
		})
	}
}

// TestCompactTriggerEmptyFirstThenContent: the report's edge case. The first message finds a
// missing transcript and compacts nothing; once the target has content the next message of the
// same caller run compacts it. A reset acts on the second message too.
func TestCompactTriggerEmptyFirstThenContent(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   int
	}{{sessionfs.ProfileMessageContextCompact, 1}, {sessionfs.ProfileMessageContextReset, 1}} {
		t.Run(tc.action, func(t *testing.T) {
			f := newLayoutFixture(t)
			compactions, resets := countCompactions(f), countResets(f)
			writeTargetTranscript(t, f, "")
			sendWithTrigger(t, f, 1, "first", contextTrigger(tc.action, 0))
			if len(*compactions)+len(*resets) != 0 {
				t.Fatalf("acted on an empty transcript: %v %v", *compactions, *resets)
			}
			writeTargetTranscript(t, f, userLine)
			sendWithTrigger(t, f, 1, "second", contextTrigger(tc.action, 0))
			if got := len(*compactions) + len(*resets); got != tc.want {
				t.Fatalf("%s after the target gained content acted %d times, want %d", tc.action, got, tc.want)
			}
		})
	}
}

// TestCompactTriggerPositiveThresholdRecrosses: with a positive after, each message reads the
// tokens afresh: above compacts, at or below does not, above again compacts again, all in one
// caller run. The token read is the transcript's, so a compaction that lowers it re-arms the gate.
func TestCompactTriggerPositiveThresholdRecrosses(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, profileUsageTranscript(5))
	compactions := countCompactions(f)
	tokens := []int64{50, 50, 10, 3, 50}
	var reads atomic.Int64
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) {
		n := tokens[reads.Add(1)-1]
		return &n, nil
	}
	var acted []int
	for i := range tokens {
		before := len(*compactions)
		sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextCompact, 10))
		if len(*compactions) > before {
			acted = append(acted, i)
		}
	}
	// 50 and 50 are above 10, 10 is not strictly above, 3 is below, 50 is above again.
	if !reflect.DeepEqual(acted, []int{0, 1, 4}) {
		t.Fatalf("compacted at messages %v, want [0 1 4]", acted)
	}
}

// TestCompactTriggerFailureIsRetriedByTheNextCall: a failed compaction is logged and the message
// is sent anyway, and no claim holds the failure: the next message of the run tries again.
func TestCompactTriggerFailureIsRetriedByTheNextCall(t *testing.T) {
	f := newLayoutFixture(t)
	logs := triggerLogs(f)
	writeTargetTranscript(t, f, userLine)
	var attempts atomic.Int64
	f.service.resolver.compactProfile = func(context.Context, string, string) error {
		if attempts.Add(1) == 1 {
			return errors.New("summary model unavailable")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		if answer, err := trySendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextCompact, 0)); err != nil || !strings.Contains(answer, "ok") {
			t.Fatalf("send %d: %q, %v (a failed compaction must not stop the message)", i, answer, err)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("compaction attempts = %d, want 2", attempts.Load())
	}
	if !strings.Contains(logs.String(), "compact failed; sending the message anyway") {
		t.Fatalf("the failure was not logged:\n%s", logs.String())
	}
}

// TestCompactTriggerQueuedMessagesEachCompactWhenStarted: two messages of one caller run queue
// behind a busy target; each compacts when the pump starts it, one after the other, with the
// first already served before the second compaction.
func TestCompactTriggerQueuedMessagesEachCompactWhenStarted(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = compactTrigger(true, 0)
	})
	writeTargetTranscript(t, p.f, userLine)
	calls := countCompactions(p.f)
	var servedAtCompaction []int
	compact := p.f.service.resolver.compactProfile
	p.f.service.resolver.compactProfile = func(ctx context.Context, session, profile string) error {
		servedAtCompaction = append(servedAtCompaction, len(p.order.served()))
		return compact(ctx, session, profile)
	}
	release := p.holdTranscript(t)
	var handles []string
	for _, text := range []string{"alpha-1 please", "alpha-2 please"} {
		out := p.sendAs(t, callerRunContext(t, p.f, 1), tools.ExecutionContext{SessionKey: p.owner}, text)
		if !strings.Contains(out, "state: queued") {
			t.Fatalf("a message to a busy target must queue:\n%s", out)
		}
		handles = append(handles, p.handle(t, out))
	}
	if len(*calls) != 0 {
		t.Fatalf("compacted while the target was held: %v", *calls)
	}
	release()
	for i, id := range handles {
		if got := p.wait(t, id); !strings.Contains(got, "answer for alpha-"+string(rune('1'+i))) {
			t.Fatalf("handle %s:\n%s", id, got)
		}
	}
	if len(*calls) != 2 || !reflect.DeepEqual(servedAtCompaction, []int{0, 1}) {
		t.Fatalf("compactions %v served at compaction %v, want two, at 0 and 1 served", *calls, servedAtCompaction)
	}
}

// TestCompactTriggerConcurrentCallsAreSerializedPerTarget: simultaneous messages of one caller run
// to one target queue on its transcript; every one compacts, and no two compactions overlap, so a
// compaction never meets the run of another message.
func TestCompactTriggerConcurrentCallsAreSerializedPerTarget(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine)
	var inFlight, overlaps, calls atomic.Int64
	f.service.resolver.compactProfile = func(context.Context, string, string) error {
		calls.Add(1)
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}
	const senders = 6
	var wg sync.WaitGroup
	errs := make(chan error, senders)
	ctx := callerRunContext(t, f, 1)
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: compactTrigger(true, 0), QueueWait: 30 * time.Second, MaxQueued: senders}
			_, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != senders || overlaps.Load() != 0 {
		t.Fatalf("compactions %d (want %d), overlapping %d (want 0)", calls.Load(), senders, overlaps.Load())
	}
}

// TestCompactTriggerSubagentsCompactOnEveryMessage: a sub-agent and its own sub-agent message the
// reviewer through the real launcher with compact on: each message compacts.
func TestCompactTriggerSubagentsCompactOnEveryMessage(t *testing.T) {
	f, _ := subagentTriggerFixture(t, map[string]any{"action": "compact"})
	compactions := countCompactions(f)
	f.script = subagentTriggerScript("mid-says-hello", "leaf-says-hello")
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	if len(reviewerBodies(f)) != 2 {
		t.Fatalf("reviewer requests = %d, want 2", len(reviewerBodies(f)))
	}
	if !reflect.DeepEqual(*compactions, []string{"review/reviewer", "review/reviewer"}) {
		t.Fatalf("compactions %v, want one per sub-agent message", *compactions)
	}
}

// TestCompactTriggerNeedsNoRunningCallerRun: a call without any caller run state (a child call
// context) compacts like any other.
func TestCompactTriggerNeedsNoRunningCallerRun(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine)
	compactions := countCompactions(f)
	for _, ctx := range []context.Context{native.WithChildCall(context.Background()), native.WithChildCall(context.Background()), native.WithChildCall(context.Background())} {
		request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: compactTrigger(true, 0)}
		if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if len(*compactions) != 3 {
		t.Fatalf("compactions %v, want 3", *compactions)
	}
}

// TestCompactTriggerWithTheRealCompactionKeepsOnlyTheLastExchange pins what a compact before every
// message does to the target with its default compaction (assistant_final, keep 1 turn, no
// summary): the older history is dropped at the first message, and at each later one the target
// keeps only its previous exchange (the active tail) plus the new message. It is not a defect: a
// target that should remember more raises after, keep_turns or turns on summary.
func TestCompactTriggerWithTheRealCompactionKeepsOnlyTheLastExchange(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	writeTargetTranscript(t, f, profileUsageTranscript(5))
	for _, text := range []string{"msgA", "msgB", "msgC"} {
		sendWithTrigger(t, f, 1, text, contextTrigger(sessionfs.ProfileMessageContextCompact, 0))
	}
	bodies := reviewerBodies(f)
	if len(bodies) != 3 {
		t.Fatalf("reviewer requests = %d, want 3", len(bodies))
	}
	for i, want := range []struct{ keeps, drops []string }{
		{[]string{"q3", "msgA"}, []string{"q1", "q2"}},
		{[]string{"msgA", `"text":"ok"`, "msgB"}, []string{"q1", "q2", "q3"}},
		{[]string{"msgB", `"text":"ok"`, "msgC"}, []string{"q3", "msgA"}},
	} {
		for _, kept := range want.keeps {
			if !strings.Contains(bodies[i], kept) {
				t.Errorf("request %d lost %q:\n%s", i, kept, bodies[i])
			}
		}
		for _, dropped := range want.drops {
			if strings.Contains(bodies[i], dropped) {
				t.Errorf("request %d still carries %q:\n%s", i, dropped, bodies[i])
			}
		}
	}
}
