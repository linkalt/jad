//go:build windows

package native

// processExists is not implemented on Windows: every process found going at startup is
// treated as gone, which is the rule before servers recorded their identity.
func processExists(int) bool { return false }

func processStartTime(int) uint64 { return 0 }
