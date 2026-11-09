//go:build !unix

package worktree

import (
	"errors"
	"os"
)

// lockOverlay fails: the overlay materialiser needs Linux.
func lockOverlay(string) (*os.File, error) {
	return nil, errors.New("the overlay materialiser runs on Linux only")
}

// tryFlock fails: the overlay materialiser needs Linux.
func tryFlock(*os.File) (bool, error) {
	return false, errors.New("the overlay materialiser runs on Linux only")
}
