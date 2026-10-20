//go:build windows

package native

import (
	"os/exec"
)

func configureProcessGroup(_ *exec.Cmd) {}

func signalProcessGroup(command *exec.Cmd, _ bool) {
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}

// Windows has no process group here: only the direct process is stopped, by the call itself.
func signalProcessGroupID(int, bool) {}

func processGroupAlive(int) bool { return false }
