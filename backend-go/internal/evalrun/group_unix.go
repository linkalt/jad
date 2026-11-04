//go:build unix

package evalrun

import (
	"os/exec"
	"syscall"
)

// startInGroup starts command in its own process group and makes its cancellation kill the
// whole group, so a check's background children end with it.
func startInGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
