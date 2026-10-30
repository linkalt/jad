//go:build !windows

package native

import (
	"errors"
	"syscall"
)

// processExists sends signal 0, which checks that the process exists without touching
// it. A process owned by another user answers "not permitted" and still exists.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
