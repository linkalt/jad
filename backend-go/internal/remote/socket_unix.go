//go:build unix

package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var processUID = os.Geteuid

// checkSocketOwner refuses a socket the bearer token must not be sent to: one owned by neither
// this user nor root, or one in a directory that group or other users can write to
// (without the sticky bit they could replace it).
func checkSocketOwner(socket string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := uint32(processUID()); stat.Uid != uid && stat.Uid != 0 {
		return fmt.Errorf("socket %s is owned by another user", socket)
	}
	parent, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return fmt.Errorf("socket %s: its directory cannot be read", socket)
	}
	if parent.Mode().Perm()&0o022 != 0 && parent.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("socket %s is in a directory that other users can write to", socket)
	}
	return nil
}
