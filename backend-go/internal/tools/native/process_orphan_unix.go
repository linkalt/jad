//go:build !windows

package native

import "syscall"

// signalGroupOfPID sends SIGTERM, or SIGKILL when force is set, to the process group
// led by pid.
func signalGroupOfPID(pid int, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, signal)
}
