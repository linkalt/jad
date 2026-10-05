//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package sessionlock

import (
	"errors"
	"os"
)

func tryAcquire(*os.File) (bool, error) {
	return false, errors.New("operating-system session locks are unsupported on this platform")
}

func release(*os.File) error {
	return errors.New("operating-system session locks are unsupported on this platform")
}
