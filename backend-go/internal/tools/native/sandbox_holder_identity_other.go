//go:build !linux

package native

import "errors"

// Overlay runs need Linux (ProbeOverlay refuses elsewhere), so there is no holder to name.

func readProcessStart(int) (uint64, byte, error) {
	return 0, 0, errors.ErrUnsupported
}

func openPidfd(int) int { return -1 }

func pidfdExited(int) bool { return false }

func closeFD(int) {}

func killProcess(processIdentity) error { return errors.ErrUnsupported }
