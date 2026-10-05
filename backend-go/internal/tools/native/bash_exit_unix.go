//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package native

import (
	"errors"
	"os/exec"
	"syscall"
)

func bashExitStatus(waitErr error) (int, bool, string) {
	if waitErr == nil {
		return 0, true, ""
	}
	var exitError *exec.ExitError
	if !errors.As(waitErr, &exitError) {
		return 0, false, ""
	}
	if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return 0, false, bashSignalName(status.Signal())
		}
		return status.ExitStatus(), true, ""
	}
	if code := exitError.ExitCode(); code >= 0 {
		return code, true, ""
	}
	return 0, false, ""
}

func bashSignalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGALRM:
		return "SIGALRM"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return signal.String()
	}
}
