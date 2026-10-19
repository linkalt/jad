package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
)

func TestMaintenanceWaitsOnTheRunLockEndWithTheContextError(t *testing.T) {
	f := newLayoutFixture(t)
	bound := boundRunnerOf(t, f, "review/reviewer")
	mutation := boundMutation{shared: bound.shared, config: bound.config}
	mutation.config.FrontCompactionConfig.Enabled = true
	mutation.config.StateContext = sessionfs.StateContextSettings{
		Enabled:  true,
		Triggers: map[string]sessionfs.StateContextTrigger{sessionfs.StateContextTriggerUser: {MaxPromptBytes: 64}},
	}

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"Compact", func(ctx context.Context) error { _, err := mutation.Compact(ctx); return err }},
		{"Reset", func(ctx context.Context) error { return mutation.Reset(ctx) }},
		{"FrontCompact", func(ctx context.Context) error { _, err := mutation.FrontCompact(ctx, "m1"); return err }},
		{"StateContext", func(ctx context.Context) error { _, err := mutation.StateContext(ctx, "hi", true); return err }},
		{"RestoreCheckpoint", func(ctx context.Context) error {
			_, err := mutation.RestoreCheckpoint(ctx, "c1", httpapi.CheckpointRestoreRequest{DryRun: true})
			return err
		}},
	}
	// Each method checks the context before it touches the lock, so a free lock with a done
	// context only reaches that early check; the re-check after LockCtx is not reachable here.
	for _, tc := range cases {
		t.Run(tc.name+"WithAFreeLockAndACancelledContext", func(t *testing.T) {
			before, _ := os.ReadFile(bound.config.MessagesPath)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for i := 0; i < 50; i++ {
				if err := tc.call(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: error = %v, want context.Canceled", i, err)
				}
			}
			if after, _ := os.ReadFile(bound.config.MessagesPath); string(after) != string(before) {
				t.Fatal("a call with a cancelled context changed the transcript")
			}
			if err := bound.shared.runMu.LockCtx(context.Background()); err != nil {
				t.Fatal(err)
			}
			bound.shared.runMu.Unlock()
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bound.shared.runMu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the call kept waiting for the run lock after its context ended")
			}
			bound.shared.runMu.Unlock()
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the lock was released by the failed call: a second Unlock did not panic")
					}
				}()
				bound.shared.runMu.Unlock()
			}()
		})
	}
}
