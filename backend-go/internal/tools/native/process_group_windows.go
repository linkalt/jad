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
