package app

import (
	"context"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
)

// TestRemoteMessageArmAfterEndStartsNoTimer checks the arm race: a run that ends before arm
// runs is already finished there, so arm starts no timer and no goroutine is left behind.
func TestRemoteMessageArmAfterEndStartsNoTimer(t *testing.T) {
	server, provider := newHeldRemoteFixture(t, 1, 60_000)
	backend := remoteBackend{resolver: server.service.resolver}
	resolved, err := backend.resolveProfile(context.Background(), "demo", "dev/p0")
	if err != nil {
		t.Fatal(err)
	}
	started, err := server.service.resolver.profileRunStarter().StartProfileRun(context.Background(), httpapi.ProfileRunRequest{
		Reference: resolved.Reference,
		Profile:   resolved.Profile,
		Caller:    "peer",
		Chain:     []string{"peer", chainNode(resolved.Reference, resolved.Profile)},
		Message:   "hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.reached
	started.Stop()
	<-started.Done()
	run := &remoteMessageRun{done: make(chan struct{})}
	run.ended(started)
	before := runtime.NumGoroutine()
	run.arm(started, time.Hour)
	run.mu.Lock()
	timer := run.timer
	run.mu.Unlock()
	if timer != nil {
		t.Fatal("arm after the run ended left a timer armed")
	}
	if after := settledGoroutineCount(before); after > before {
		t.Fatalf("goroutines %d -> %d after arm on an ended run", before, after)
	}
}
