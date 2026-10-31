// Package fserr holds helpers for file-system errors that are shown to users.
package fserr

import (
	"errors"
	"io/fs"
)

// UnwrapPathError drops the *fs.PathError wrapper, which repeats the path, so the
// message stays short. An error without a PathError is returned unchanged.
func UnwrapPathError(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
