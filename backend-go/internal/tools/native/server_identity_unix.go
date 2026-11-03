//go:build !windows

package native

import (
	"errors"
	"syscall"
)

// processExists sends signal 0, which checks that the process exists without touching
// it. A process owned by another user answers "not permitted" and still exists. A zombie
// (exited, not yet reaped) answers too, but it has ended, so where the system reports the
// state it does not exist here.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !processIsZombie(pid)
}
