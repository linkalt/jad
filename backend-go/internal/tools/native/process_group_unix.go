//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package native

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcessGroup(command *exec.Cmd, force bool) {
	if command == nil || command.Process == nil {
		return
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-command.Process.Pid, signal); err != nil {
		_ = command.Process.Signal(signal)
	}
}
