//go:build unix

package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockOverlay takes the lock of the overlay run whose layers are in dir, without waiting: a
// lock another holder has means a mount of the layers may still exist.
func lockOverlay(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, overlayLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the run's overlay lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrHeld, dir)
		}
		return nil, fmt.Errorf("lock the run's overlay: %w", err)
	}
	return file, nil
}

// tryFlock takes an exclusive flock on file without waiting; false means another open file
// description holds it.
func tryFlock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
