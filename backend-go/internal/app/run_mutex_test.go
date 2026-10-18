package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunMutexZeroValueLocksAndUnlocks(t *testing.T) {
	var mu runMutex
	mu.Lock()
	if mu.TryLock() {
		t.Fatal("TryLock succeeded on a held mutex")
	}
	mu.Unlock()
	if !mu.TryLock() {
		t.Fatal("TryLock failed on a free mutex")
	}
	mu.Unlock()
}

func TestRunMutexLockWaitsForUnlock(t *testing.T) {
	var mu runMutex
	mu.Lock()
	time.AfterFunc(30*time.Millisecond, mu.Unlock)
	if err := mu.LockCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mu.TryLock() {
		t.Fatal("LockCtx returned without holding the mutex")
	}
	mu.Unlock()
}

func TestRunMutexLockCtxReturnsContextErrorWhileHeld(t *testing.T) {
	var mu runMutex
	mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- mu.LockCtx(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LockCtx kept waiting after its context was cancelled")
	}
	mu.Unlock()
	if !mu.TryLock() {
		t.Fatal("the mutex was not free after the holder unlocked")
	}
	mu.Unlock()
}

func TestRunMutexCancelledLockCtxDoesNotTakeTheLock(t *testing.T) {
	var mu runMutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mu.LockCtx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !mu.TryLock() {
		t.Fatal("a cancelled LockCtx took the lock")
	}
	mu.Unlock()
}

func TestRunMutexUnlockOnFreeMutexPanics(t *testing.T) {
	var mu runMutex
	defer func() {
		if got := recover(); got != "app: unlock of unlocked runMutex" {
			t.Fatalf("panic = %v, want the unlock of unlocked runMutex panic", got)
		}
	}()
	mu.Unlock()
	t.Fatal("Unlock on a free mutex did not panic")
}
