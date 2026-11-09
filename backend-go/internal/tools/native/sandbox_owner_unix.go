//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package native

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether this process's user owns the file.
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
