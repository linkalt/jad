package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"jad/backend-go/internal/tools/native"
)

const reviewerOwner = "local:demo\x00review/reviewer"

func reviewerContext(t *testing.T, f *layoutFixture) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), callerMessagesPathKey{}, boundRunnerOf(t, f, "review/reviewer").config.MessagesPath)
}

func stopReviewer(t *testing.T, f *layoutFixture) {
	t.Helper()
	f.mu.Lock()
	f.hold = false
	f.mu.Unlock()
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("review/reviewer"), ""); status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
}

func TestProfileMessageChainRefusesALoopWhateverTheCap(t *testing.T) {
	f := newLayoutFixture(t)
	done := startHeldProfileMessage(t, f, context.Background())
	// dev/coder -> review/reviewer is running; the reviewer messaging dev/coder closes A -> B -> A.
	for _, limit := range []int{0, 2, 8} {
		ctx := reviewerContext(t, f)
		if limit > 0 {
			ctx = native.WithProfileChainMax(ctx, limit)
		}
		if _, err := (profileMessenger{resolver: f.service.resolver}).Message(ctx, reviewerOwner, "dev/coder", "hi", 0, false); !errors.Is(err, native.ErrProfileCycle) {
			t.Errorf("max_chain %d: error = %v, want ErrProfileCycle", limit, err)
		}
	}
	stopReviewer(t, f)
	awaitMessageEnd(t, done)
}

func TestProfileMessageChainCapStopsAThirdProfile(t *testing.T) {
	f := newLayoutFixture(t)
	f.overwrite(t, "profiles/review/helper.json", `{"models":["openai/large"],"toolset":"coding/none"}`)
	done := startHeldProfileMessage(t, f, context.Background())
	// The chain is [dev/coder, review/reviewer]; adding review/helper makes three.
	_, err := (profileMessenger{resolver: f.service.resolver}).Message(reviewerContext(t, f), reviewerOwner, "review/helper", "hi", 0, false)
	if !errors.Is(err, native.ErrProfileChainTooLong) {
		t.Fatalf("default max_chain: error = %v, want ErrProfileChainTooLong", err)
	}
	// Raising the cap lets it through to the start; the held provider keeps the run going.
	ctx, cancel := context.WithCancel(native.WithProfileChainMax(reviewerContext(t, f), 3))
	third := make(chan error, 1)
	go func() {
		_, err := (profileMessenger{resolver: f.service.resolver}).Message(ctx, reviewerOwner, "review/helper", "hi", 0, false)
		third <- err
	}()
	cancel()
	if err := awaitMessageEnd(t, third); errors.Is(err, native.ErrProfileChainTooLong) || errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("max_chain 3: error = %v, want the chain to be accepted", err)
	}
	stopReviewer(t, f)
	awaitMessageEnd(t, done)
}

func TestProfileMessageFromASubAgentUsesTheSessionsProfileAsCaller(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	// A child's context carries no transcript path; it is marked as a child call instead.
	ctx := native.WithChildCall(context.Background())
	if _, err := messenger.Message(ctx, profileMessageOwner, "dev/coder", "hi", 0, false); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("a child of dev/coder messaging dev/coder: error = %v, want ErrProfileCycle", err)
	}
	if _, err := messenger.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi", 0, false); err == nil {
		t.Fatal("a call without a transcript path and not from a child must still fail closed")
	}
	reply, err := messenger.Message(ctx, profileMessageOwner, "review/reviewer", "hi", 0, false)
	if err != nil || reply == "" {
		t.Fatalf("a child of dev/coder messaging review/reviewer = %q, %v; want the reviewer's answer", reply, err)
	}
}
