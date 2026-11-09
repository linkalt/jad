//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package native

import "os"

// ownedByCurrentUser is true where files carry no owner jad can compare; bwrap and seatbelt,
// the backends that use the {tmp} folders, run only on Linux and macOS.
func ownedByCurrentUser(os.FileInfo) bool { return true }
