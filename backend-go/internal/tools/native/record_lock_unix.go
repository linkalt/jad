//go:build unix

package native

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockRecord takes an exclusive flock on the lock file at path (a record's own lock file: the
// record is rewritten by rename, which would leave a lock on its old inode) without waiting, so that two
// servers on one state folder do not collect the same worktree at once. It returns nil, nil
// when another process holds the lock; the caller closes the file to let go.
func lockRecord(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return file, nil
}
