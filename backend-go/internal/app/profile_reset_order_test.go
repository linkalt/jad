package app

import (
	"context"
	"testing"
	"time"
)

// A reset waits for the profile's transcript lock while another process holds it. While it
// waits, the mutation gate is held, so a run that starts meanwhile is refused; once the lock
// is free the reset runs, and both the gate and the lock are free again when it returns.
func TestResetHoldsTheGateWhileItWaitsForAnotherProcessAndClearsIt(t *testing.T) {
	rootSetting := newSharedSessionRoot(t)
	other := startLockHelper(t, rootSetting, "hold:dev/blue")
	other.expect(t, "held")
	resolver, err := resolverForRoot(rootSetting)
	if err != nil {
		t.Fatal(err)
	}
	key := sessionKey("session-1", "dev/blue")
	finished := make(chan error, 1)
	go func() {
		finished <- resolver.ResetProfile(context.Background(), "session-1", "dev/blue")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !resolver.gate.Held(key) {
		if time.Now().After(deadline) {
			t.Fatal("the reset did not take the gate while it waited for the other process")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-finished:
		t.Fatalf("the reset finished while another process held the transcript: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if resolver.gate.Begin(key) {
		t.Fatal("a second mutation took the gate while the reset held it")
	}
	other.finish(t)
	if err := <-finished; err != nil {
		t.Fatalf("reset after the other process released: %v", err)
	}
	if resolver.gate.Held(key) {
		t.Fatal("the gate stayed set after the reset returned")
	}
	release, err := lockInRoot(resolver, "dev/blue", time.Second)
	if err != nil {
		t.Fatalf("the transcript lock stayed held after the reset: %v", err)
	}
	release()
}

// A reset spelled differently from the session's canonical reference takes the in-process
// session lock under the canonical key, the one a run start and the other maintenance take, so
// a second maintenance of that session waits for it. local:session-1 and session-1 are one
// session on a single root.
func TestResetLocksTheSessionUnderItsCanonicalReference(t *testing.T) {
	rootSetting := newSharedSessionRoot(t)
	other := startLockHelper(t, rootSetting, "hold:dev/blue")
	other.expect(t, "held")
	resolver, err := resolverForRoot(rootSetting)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- resolver.ResetProfile(context.Background(), "local:session-1", "dev/blue")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !resolver.gate.Held(sessionKey("session-1", "dev/blue")) {
		if time.Now().After(deadline) {
			t.Fatal("the reset did not take the gate while it waited for the other process")
		}
		time.Sleep(time.Millisecond)
	}
	locked := make(chan struct{})
	go func() {
		unlock := resolver.gate.Lock("session-1")
		unlock()
		close(locked)
	}()
	select {
	case <-locked:
		other.finish(t)
		<-finished
		t.Fatal("a maintenance of the canonical session took the session lock during a reset spelled local:session-1")
	case <-time.After(100 * time.Millisecond):
	}
	other.finish(t)
	if err := <-finished; err != nil {
		t.Fatalf("reset after the other process released: %v", err)
	}
	<-locked
}
