//go:build unix

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const shutdownChildEnv = "JAD_TEST_SHUTDOWN_SIGNAL_CHILD"

// The first signal starts the shutdown, which may wait minutes for the async handles; a
// second one must end the process instead of being swallowed by the first one's handler.
func TestSecondSignalEndsTheProcessDuringShutdown(t *testing.T) {
	if os.Getenv(shutdownChildEnv) == "1" {
		ctx, stop := notifyShutdown()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		// A shutdown that waits longer than the parent does.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSecondSignalEndsTheProcessDuringShutdown$")
	child.Env = append(os.Environ(), shutdownChildEnv+"=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	lines := bufio.NewScanner(stdout)
	for lines.Scan() && strings.TrimSpace(lines.Text()) != "ready" {
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		_ = child.Process.Signal(syscall.SIGINT)
		select {
		case err := <-exited:
			if err == nil {
				t.Fatal("the child finished its shutdown instead of ending at the second signal")
			}
			return
		case <-deadline:
			t.Fatal("repeated SIGINTs during shutdown did not end the process")
		case <-tick.C:
		}
	}
}
