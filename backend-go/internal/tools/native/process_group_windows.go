//go:build windows

package native

import (
	"os/exec"
	"time"
)

func configureProcessGroup(_ *exec.Cmd) {}

func signalProcessGroup(command *exec.Cmd, _ bool) {
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}

// Windows has no process group here: only the direct process is stopped, by the call itself.
func killGroupAfter(command *exec.Cmd, grace time.Duration) *time.Timer {
	return time.AfterFunc(grace, func() { signalProcessGroup(command, true) })
}

// Only the direct process exists here, and the caller cancels after it was reaped.
func cancelKillIfGroupGone(timer *time.Timer, _ *exec.Cmd) {
	if timer != nil {
		timer.Stop()
	}
}

func signalProcessGroupID(int, bool) {}

func processGroupAlive(int) bool { return false }
