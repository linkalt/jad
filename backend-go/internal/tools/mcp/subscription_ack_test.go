package mcp

import (
	"testing"
	"time"
)

// An acknowledgement wakes the waiters of the URIs it lists on that server only; a waiter
// that gave up is forgotten.
func TestSubscriptionAcksWakeTheAcknowledgedURIs(t *testing.T) {
	var acks subscriptionAcks
	plan := acks.wait("docs", "docs://plan")
	other := acks.wait("other", "docs://plan")
	gaveUp := acks.wait("docs", "docs://old")
	acks.forget("docs", "docs://old", gaveUp)
	acks.acknowledge("docs", []string{"docs://plan", "docs://unknown"})
	select {
	case <-plan:
	case <-time.After(time.Second):
		t.Fatal("the acknowledged URI's waiter was not woken")
	}
	select {
	case <-other:
		t.Fatal("another server's waiter was woken")
	default:
	}
	if len(acks.waiters) != 1 {
		t.Fatalf("waiters left = %d, want only the other server's", len(acks.waiters))
	}
}
