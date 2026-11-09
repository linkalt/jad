//go:build !unix

package native

import "os"

// lockRecord is not available here: the record is tried without a lock (Windows runs no
// overlay and has no second server on a state folder to guard against).
func lockRecord(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
