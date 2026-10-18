package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

const profileMessageOwner = "local:demo\x00dev/coder"

func TestProfileMessageDeliversToTheTargetTranscriptAndSecondCallSeesTheFirst(t *testing.T) {
	f := newLayoutFixture(t)
	f.captureBodies = true
	messenger := profileMessenger{resolver: f.service.resolver}

	answer, err := messenger.Message(context.Background(), profileMessageOwner, "review/reviewer", "first question")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "ok") {
		t.Fatalf("answer = %q, want the provider's reply", answer)
	}
	reviewerMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "messages.jsonl")
	data, err := os.ReadFile(reviewerMessages)
	if err != nil || !strings.Contains(string(data), "first question") || !strings.Contains(string(data), "ok") {
		t.Fatalf("transcript of review/reviewer: %v %s", err, data)
	}
	coderMessages := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "messages.jsonl")
	if data, err := os.ReadFile(coderMessages); err == nil && strings.Contains(string(data), "first question") {
		t.Fatalf("the caller's transcript received the message: %s", data)
	}

	if _, err := messenger.Message(context.Background(), profileMessageOwner, "review/reviewer", "second question"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.bodies[len(f.bodies)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "first question") || !strings.Contains(last, "second question") {
		t.Fatalf("second request does not carry the first exchange: %s", last)
	}
}

func TestProfileMessageToABusyTargetFailsFast(t *testing.T) {
	f := newLayoutFixture(t)
	release, err := f.service.resolver.LockSession(context.Background(), "local:demo", "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = profileMessenger{resolver: f.service.resolver}.Message(context.Background(), profileMessageOwner, "review/reviewer", "hi")
	if !errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want ErrProfileBusy", err)
	}
}

func TestProfileMessageRejectsAnOwnerKeyWithoutSeparator(t *testing.T) {
	f := newLayoutFixture(t)
	_, err := profileMessenger{resolver: f.service.resolver}.Message(context.Background(), "local:demo", "review/reviewer", "hi")
	if err == nil || errors.Is(err, native.ErrProfileCycle) || errors.Is(err, native.ErrProfileBusy) {
		t.Fatalf("error = %v, want a plain error", err)
	}
}

func TestRecordProfileRunFailureRecordsUnlessCancelled(t *testing.T) {
	for _, c := range []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		stop       func(cancel context.CancelFunc)
		recorded   bool
	}{
		{"timeout", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, func(context.CancelFunc) {}, true},
		{"cancel", func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, func(cancel context.CancelFunc) { cancel() }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newLayoutFixture(t)
			f.mu.Lock()
			f.hold, f.reached = true, make(chan struct{}, 4)
			f.mu.Unlock()
			ctx, cancel := c.newContext()
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := profileMessenger{resolver: f.service.resolver}.Message(ctx, profileMessageOwner, "review/reviewer", "hi")
				done <- err
			}()
			select {
			case <-f.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("the run never reached the provider")
			}
			c.stop(cancel)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("run succeeded, want an error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end")
			}
			cache, _ := os.ReadFile(filepath.Join(f.cwd, ".jad", "state", "demo", "review", "reviewer", "staging", "run-error-cache.json"))
			if got := strings.Contains(string(cache), profileRunFailureText); got != c.recorded {
				t.Fatalf("run error recorded = %v, want %v; cache: %s", got, c.recorded, cache)
			}
		})
	}
}

func TestProfileMessageRefusesNestedCallsAndTheCallerItself(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}

	nested := context.WithValue(context.Background(), profileChainKey{}, []string{"dev/coder", "review/reviewer"})
	if _, err := messenger.Message(nested, profileMessageOwner, "dev/coder", "hi"); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("nested call error = %v, want ErrProfileCycle", err)
	}
	if _, err := messenger.Message(context.Background(), profileMessageOwner, "dev/coder", "hi"); !errors.Is(err, native.ErrProfileCycle) {
		t.Fatalf("self call error = %v, want ErrProfileCycle", err)
	}
}
