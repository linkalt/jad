//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package native

import (
	"os/exec"
	"syscall"
	"time"
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

// killGroupAfter sends the hard kill after grace, but only while the group still has a member,
// so the id of a finished group (which the system may hand out again) is never signalled.
func killGroupAfter(command *exec.Cmd, grace time.Duration) *time.Timer {
	pgid := command.Process.Pid
	return time.AfterFunc(grace, func() {
		if processGroupAlive(pgid) {
			signalProcessGroup(command, true)
		}
	})
}

// cancelKillIfGroupGone stops the timer once the leader is reaped and no member is left.
func cancelKillIfGroupGone(timer *time.Timer, command *exec.Cmd) {
	if timer != nil && !processGroupAlive(command.Process.Pid) {
		timer.Stop()
	}
}

func signalProcessGroupID(pgid int, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-pgid, signal)
}

// processGroupAlive reports whether group pgid has a member that has not exited. kill(-pgid, 0)
// also succeeds for a group of unreaped zombies (orphans reparented to an init that does not
// reap them), which would make every stop wait its whole grace; where /proc says every member
// is a zombie, the group is gone.
func processGroupAlive(pgid int) bool {
	if pgid <= 1 || syscall.Kill(-pgid, 0) != nil {
		return false
	}
	if live, known := groupHasLiveMember(pgid); known {
		return live
	}
	return true
}
