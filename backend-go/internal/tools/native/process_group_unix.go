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

func signalProcessGroupID(pgid int, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-pgid, signal)
}

func processGroupAlive(pgid int) bool {
	return pgid > 1 && syscall.Kill(-pgid, 0) == nil
}
