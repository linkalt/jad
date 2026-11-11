package app

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// A reset context_trigger is evaluated on every message that starts a run of the target, like a
// compact: it keeps no per-caller-run state. It is destructive, so each qualifying message
// really does drop what the target held, including what the message before it just wrote.

func bodyOf(f *layoutFixture, i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

// TestResetTriggerActsOnEveryCallOfOneCallerRun: three messages of one caller run to one target
// reset it three times at after 0, and each request carries only its own message.
func TestResetTriggerActsOnEveryCallOfOneCallerRun(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messages := writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	failOnUsageRead(t, f)
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	for i, text := range []string{"msg-one", "msg-two", "msg-three"} {
		sendWithTrigger(t, f, 1, text, reset)
		body := bodyOf(f, i)
		if !strings.Contains(body, text) || strings.Contains(body, "old question") {
			t.Fatalf("request %d: %s", i, body)
		}
		if i > 0 && strings.Contains(body, []string{"msg-one", "msg-two"}[i-1]) {
			t.Fatalf("request %d still carries the message before it (no reset before it): %s", i, body)
		}
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "msg-one") || strings.Contains(string(data), "msg-two") || !strings.Contains(string(data), "msg-three") {
		t.Fatalf("messages.jsonl after three resets:\n%s", data)
	}
}

// TestResetTriggerEmptyFirstThenContentResetsTheSecond: an empty target at the first message is
// left alone (nothing to reset, no claim spent), and the next message of the run resets what
// the first one wrote.
func TestResetTriggerEmptyFirstThenContentResetsTheSecond(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	resets := countResets(f)
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	sendWithTrigger(t, f, 1, "first-msg", reset)
	if len(*resets) != 0 {
		t.Fatalf("reset of a missing transcript: %v", *resets)
	}
	sendWithTrigger(t, f, 1, "second-msg", reset)
	if len(*resets) != 1 {
		t.Fatalf("resets after the target gained content = %v, want 1", *resets)
	}
}

// TestResetTriggerEmptyFirstThenRealResetDropsTheFirstMessage: the same with the real reset.
func TestResetTriggerEmptyFirstThenRealResetDropsTheFirstMessage(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	sendWithTrigger(t, f, 1, "first-msg", reset)
	sendWithTrigger(t, f, 1, "second-msg", reset)
	if body := bodyOf(f, 1); strings.Contains(body, "first-msg") || !strings.Contains(body, "second-msg") {
		t.Fatalf("second request: %s", body)
	}
}

// TestResetTriggerPositiveThresholdRecrosses: with a positive after each message reads the
// tokens afresh; above resets, at or below does not, above again resets again, in one run.
func TestResetTriggerPositiveThresholdRecrosses(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, profileUsageTranscript(5))
	resets := countResets(f)
	tokens := []int64{50, 50, 10, 3, 50}
	var reads atomic.Int64
	f.service.resolver.profileUsage = func(context.Context, string, string) (*int64, error) {
		n := tokens[reads.Add(1)-1]
		return &n, nil
	}
	var acted []int
	for i := range tokens {
		before := len(*resets)
		sendWithTrigger(t, f, 1, "hi", contextTrigger(sessionfs.ProfileMessageContextReset, 10))
		if len(*resets) > before {
			acted = append(acted, i)
		}
	}
	if !reflect.DeepEqual(acted, []int{0, 1, 4}) {
		t.Fatalf("reset at messages %v, want [0 1 4]", acted)
	}
}

// TestResetTriggerFailureIsRetriedAndLaterCallsResetAgain: a failed reset fails the message
// closed (nothing is sent); the next message tries again, and the one after it resets again too.
func TestResetTriggerFailureIsRetriedAndLaterCallsResetAgain(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	var attempts int
	f.service.resolver.resetProfile = func(ctx context.Context, reference, profile string) error {
		attempts++
		if attempts == 1 {
			return errors.New("disk on fire")
		}
		return f.service.resolver.resetTarget(ctx, reference, profile)
	}
	reset := contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	_, err := trySendWithTrigger(t, f, 1, "failed-msg", reset)
	var triggerErr *native.ContextTriggerError
	if !errors.As(err, &triggerErr) || triggerErr.Action != sessionfs.ProfileMessageContextReset {
		t.Fatalf("first send error = %v, want the reset's ContextTriggerError", err)
	}
	f.mu.Lock()
	sent := len(f.bodies)
	f.mu.Unlock()
	if sent != 0 {
		t.Fatalf("the failed reset still sent the message (%d requests)", sent)
	}
	sendWithTrigger(t, f, 1, "retry-msg", reset)
	sendWithTrigger(t, f, 1, "later-msg", reset)
	if attempts != 3 {
		t.Fatalf("reset attempts = %d, want 3 (the failure, the retry, the next call)", attempts)
	}
	if body := bodyOf(f, 0); strings.Contains(body, "old question") || !strings.Contains(body, "retry-msg") {
		t.Fatalf("retry request: %s", body)
	}
	if body := bodyOf(f, 1); strings.Contains(body, "retry-msg") || !strings.Contains(body, "later-msg") {
		t.Fatalf("the call after the retry did not reset again: %s", body)
	}
}

// TestResetTriggerQueuedMessagesEachResetWhenStarted: two messages queue behind a busy target;
// each resets when the pump starts it, the first already served before the second reset.
func TestResetTriggerQueuedMessagesEachResetWhenStarted(t *testing.T) {
	p := newPMFixture(t, func(settings *sessionfs.NativeProfileMessageSettings) {
		settings.ContextTrigger = contextTrigger(sessionfs.ProfileMessageContextReset, 0)
	})
	messages := writeTargetTranscript(t, p.f, strings.ReplaceAll(profileUsageTranscript(5), "q", "old-secret-q"))
	var mu sync.Mutex
	var servedAtReset []int
	p.f.service.resolver.resetProfile = func(ctx context.Context, reference, profile string) error {
		mu.Lock()
		servedAtReset = append(servedAtReset, len(p.order.served()))
		mu.Unlock()
		return p.f.service.resolver.resetTarget(ctx, reference, profile)
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
	mu.Lock()
	early := len(servedAtReset)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("reset while the target was held: %v", servedAtReset)
	}
	release()
	for i, id := range handles {
		if got := p.wait(t, id); !strings.Contains(got, fmt.Sprintf("answer for alpha-%d", i+1)) {
			t.Fatalf("handle %s:\n%s", id, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(servedAtReset, []int{0, 1}) {
		t.Fatalf("served at each reset = %v, want [0 1]", servedAtReset)
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "old-secret") || strings.Contains(string(data), "alpha-1") || !strings.Contains(string(data), "alpha-2") {
		t.Fatalf("messages.jsonl (the second reset drops the first answer):\n%s", data)
	}
}

// TestResetTriggerConcurrentCallsAreSerializedPerTarget: simultaneous messages queue on the
// target's transcript; every one resets, and no two resets overlap.
func TestResetTriggerConcurrentCallsAreSerializedPerTarget(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine)
	var inFlight, overlaps, calls atomic.Int64
	f.service.resolver.resetProfile = func(context.Context, string, string) error {
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
			request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: contextTrigger(sessionfs.ProfileMessageContextReset, 0), QueueWait: 30 * time.Second, MaxQueued: senders}
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
		t.Fatalf("resets %d (want %d), overlapping %d (want 0)", calls.Load(), senders, overlaps.Load())
	}
}

// TestResetTriggerSubagentsResetOnEveryMessage: a sub-agent and its own sub-agent message the
// reviewer through the real launcher with reset on: the child's message resets the old history,
// and the grandchild's message resets again, so it no longer sees the child's.
func TestResetTriggerSubagentsResetOnEveryMessage(t *testing.T) {
	f, messages := subagentTriggerFixture(t, map[string]any{"action": "reset"})
	f.script = subagentTriggerScript("mid-says-hello", "leaf-says-hello")
	if status, body := f.do(t, "POST", "/api/sessions/local:demo/run", `{"input":"go"}`); status != 200 || !strings.Contains(body, "event: completed") {
		t.Fatalf("run: %d %s", status, body)
	}
	bodies := reviewerBodies(f)
	if len(bodies) != 2 {
		t.Fatalf("reviewer requests = %d, want 2:\n%v", len(bodies), bodies)
	}
	if strings.Contains(bodies[0], "old-secret") || !strings.Contains(bodies[0], "mid-says-hello") {
		t.Fatalf("the child's message did not reset the reviewer first:\n%s", bodies[0])
	}
	if strings.Contains(bodies[1], "old-secret") || strings.Contains(bodies[1], "mid-says-hello") || !strings.Contains(bodies[1], "leaf-says-hello") {
		t.Fatalf("the grandchild's message did not reset again:\n%s", bodies[1])
	}
	data, _ := os.ReadFile(messages)
	if strings.Contains(string(data), "mid-says-hello") || !strings.Contains(string(data), "leaf-says-hello") {
		t.Fatalf("reviewer's messages.jsonl:\n%s", data)
	}
}

// TestResetTriggerNeedsNoCallerRun: a call with no caller run state at all (a child call context)
// resets like any other; there is no run to tell a first message from a later one any more.
func TestResetTriggerNeedsNoCallerRun(t *testing.T) {
	f := newLayoutFixture(t)
	writeTargetTranscript(t, f, userLine+ctxCompletedLine("old", 5))
	resets := countResets(f)
	ctx := native.WithChildCall(context.Background())
	for i := 0; i < 3; i++ {
		request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi", ContextTrigger: contextTrigger(sessionfs.ProfileMessageContextReset, 0)}
		if _, err := (profileMessenger{resolver: f.service.resolver}).send(ctx, request); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if len(*resets) != 3 {
		t.Fatalf("resets = %v, want 3", *resets)
	}
}
