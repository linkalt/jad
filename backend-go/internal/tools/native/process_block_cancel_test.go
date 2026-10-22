package native

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		time.Sleep(25 * time.Millisecond)
		if m := runtime.NumGoroutine(); m == n {
			break
		} else {
			n = m
		}
	}
	return n
}

// While the process runs, a cancelled blocking start must hold no more goroutines than a
// start that never blocked.
func TestCancelledBlockingProcessCallLeavesNoWaiterGoroutine(t *testing.T) {
	f := newProcessAsyncFixture(t)
	exec := tools.ExecutionContext{CallID: "p1", WorkspaceRoot: f.dir, SessionKey: "s1"}
	if _, err := f.process.Execute(context.Background(), map[string]any{"command": "sleep 30", "block_ms": json.Number("0")}, exec); err != nil {
		t.Fatal(err)
	}
	unblocked := settledGoroutines()
	f.host.Manager().CancelOwner("s1", "user_stop")
	f.wait(t, "s1", 5000)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.process.Execute(ctx, map[string]any{"command": "sleep 30", "block_ms": json.Number("100000")}, exec)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled call returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	if blocked := settledGoroutines(); blocked > unblocked {
		t.Fatalf("goroutines = %d after a cancelled blocking call, %d for a call that did not block: the waiter outlived the call", blocked, unblocked)
	}
	f.host.Manager().CancelOwner("s1", "user_stop")
}
