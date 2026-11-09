//go:build !linux

package native

import (
	"errors"
	"os"
)

// openInRoot fails: overlay runs, whose files it reads, need Linux.
func openInRoot(root, path string) (*os.File, error) {
	return nil, &os.PathError{Op: "openat2", Path: path, Err: errors.ErrUnsupported}
}
