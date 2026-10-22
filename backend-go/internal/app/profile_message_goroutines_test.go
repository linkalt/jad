package app

import (
	"context"
	"net/http"
	"net/url"
	"runtime"
	"testing"
	"time"
)

func settledGoroutineCount(limit int) int {
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > limit && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// Messages to a target that finish, are stopped by the user, and are cut by the caller's
// context must not leave goroutines behind.
func TestProfileMessageCyclesLeaveNoGoroutines(t *testing.T) {
	f := newLayoutFixture(t)
	f.cancelled = make(chan struct{}, 64)
	cycle := func() {
		if _, err := (profileMessenger{resolver: f.service.resolver}).Message(callerContext(t, f, context.Background()), profileMessageOwner, "review/reviewer", "hi", 0, false); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := startHeldProfileMessage(t, f, ctx)
		cancel()
		awaitMessageEnd(t, done)
		awaitSignal(t, f.cancelled, "the held request to be cancelled")
		awaitTargetSessionFree(t, f)

		done = startHeldProfileMessage(t, f, context.Background())
		if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("review/reviewer"), ""); status != http.StatusOK {
			t.Fatalf("stop: %d %s", status, body)
		}
		awaitMessageEnd(t, done)
		awaitSignal(t, f.cancelled, "the held request to be cancelled")
		awaitTargetSessionFree(t, f)
		f.mu.Lock()
		f.hold = false
		f.mu.Unlock()
	}
	cycle()
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 10 {
		cycle()
	}
	http.DefaultClient.CloseIdleConnections()
	if after := settledGoroutineCount(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after 10 profile_message cycles\n%s", before, after, buffer[:runtime.Stack(buffer, true)])
	}
}
