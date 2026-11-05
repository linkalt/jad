package native

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

func baselineCount() int {
	time.Sleep(200 * time.Millisecond)
	return runtime.NumGoroutine()
}

func settledCount(limit int) int {
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > limit && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

func requireNoGrowth(t *testing.T, before int, what string) {
	t.Helper()
	if after := settledCount(before); after > before {
		buffer := make([]byte, 1<<16)
		t.Fatalf("goroutines %d -> %d after %s\n%s", before, after, what, buffer[:runtime.Stack(buffer, true)])
	}
}

// 50 foreground bash calls cancelled while the child is running must release every
// reader, waiter and timer goroutine of the call.
func TestCancelledBashCallsLeaveNoGoroutines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell required")
	}
	run := func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		child := runBashCommand(ctx, bashChildRequest{
			file:        "/bin/sh",
			args:        []string{"-c", "sleep 30"},
			cwd:         t.TempDir(),
			environment: []string{"PATH=/usr/bin:/bin"},
			killGrace:   50 * time.Millisecond,
			collector:   newBashOutputCollector(bashStreamLimits{headBytes: 64, tailBytes: 64}, bashStreamLimits{headBytes: 64, tailBytes: 64}, 200, false),
		})
		if child.spawnError != nil {
			t.Skip("cannot start /bin/sh: " + child.spawnError.Error())
		}
		if !child.aborted {
			t.Fatalf("call was not aborted: %+v", child)
		}
	}
	run()
	before := baselineCount()
	for index := 0; index < 50; index++ {
		run()
	}
	requireNoGrowth(t, before, "50 cancelled bash calls")
}
