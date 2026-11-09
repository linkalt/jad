//go:build linux

package native

import (
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// readProcessStart is the start time and state of the process with pid.
func readProcessStart(pid int) (uint64, byte, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	return parseProcessStat(string(data))
}

// openPidfd is pidfd_open for pid, -1 where the kernel has none.
func openPidfd(pid int) int {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1
	}
	return fd
}

// pidfdExited reports whether the process behind fd has ended (the descriptor is readable).
func pidfdExited(fd int) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}

func closeFD(fd int) { _ = unix.Close(fd) }

// killProcess sends SIGKILL through the pidfd when there is one, which cannot reach another
// process; without one it sends it to the pid just after alive checked it.
func killProcess(p processIdentity) error {
	if p.pidfd >= 0 {
		if err := unix.PidfdSendSignal(p.pidfd, unix.SIGKILL, nil, 0); err != nil && err != unix.ESRCH {
			return err
		}
		return nil
	}
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
