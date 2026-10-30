//go:build !linux && !windows

package native

// processStartTime is not read on this system; the owner check then compares the pid only,
// so a pid reused by another program looks like the server that wrote the manifest.
func processStartTime(int) uint64 { return 0 }
