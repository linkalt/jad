//go:build windows

package native

// Managed processes need POSIX process groups, so there is never an orphan to stop here.
func signalGroupOfPID(int, bool) {}
