//go:build windows

package native

import (
	"errors"
	"os/exec"
)

func bashExitStatus(waitErr error) (int, bool, string) {
	if waitErr == nil {
		return 0, true, ""
	}
	var exitError *exec.ExitError
	if !errors.As(waitErr, &exitError) {
		return 0, false, ""
	}
	code := exitError.ExitCode()
	return code, code >= 0, ""
}
