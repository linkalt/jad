package asyncmgr

import "os"

// mkdirAll creates dir owner-only and tightens one that already exists.
func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
