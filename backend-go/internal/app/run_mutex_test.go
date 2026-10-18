package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func runMutexHeld(mu *runMutex) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := mu.LockCtx(ctx); err != nil {
		return true
	}
	mu.Unlock()
	return false
}

func TestRunMutexZeroValueLocksAndUnlocks(t *testing.T) {
	var mu runMutex
	mu.Lock()
	if !runMutexHeld(&mu) {
		t.Fatal("a held mutex was taken")
	}
	mu.Unlock()
	if runMutexHeld(&mu) {
		t.Fatal("a free mutex could not be taken")
	}
}

func TestRunMutexLockWaitsForUnlock(t *testing.T) {
	var mu runMutex
	mu.Lock()
	time.AfterFunc(30*time.Millisecond, mu.Unlock)
	if err := mu.LockCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runMutexHeld(&mu) {
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
	if runMutexHeld(&mu) {
		t.Fatal("the mutex was not free after the holder unlocked")
	}
}

func TestRunMutexCancelledLockCtxDoesNotTakeTheLock(t *testing.T) {
	var mu runMutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mu.LockCtx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if runMutexHeld(&mu) {
		t.Fatal("a cancelled LockCtx took the lock")
	}
}

func TestRunMutexExcludesConcurrentHolders(t *testing.T) {
	var mu runMutex
	const workers, rounds = 32, 200
	counter := 0
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(useCtx bool) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if useCtx {
					if err := mu.LockCtx(context.Background()); err != nil {
						t.Error(err)
						return
					}
				} else {
					mu.Lock()
				}
				counter++
				mu.Unlock()
			}
		}(w%2 == 0)
	}
	wg.Wait()
	if counter != workers*rounds {
		t.Fatalf("counter = %d, want %d", counter, workers*rounds)
	}
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
