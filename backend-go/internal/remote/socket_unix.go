//go:build unix

package remote

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

var processUID = os.Geteuid

// peerUID reads the owner of the process at the other end of a connected unix socket.
var peerUID = func(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// checkPeerOwner refuses a connected socket whose peer is owned by neither this user nor root.
// It runs after the dial, so the peer that answers is checked, not only the path that was dialed.
func checkPeerOwner(socket string, conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil
	}
	uid, err := peerUID(unixConn)
	if err != nil {
		return fmt.Errorf("socket %s: its peer cannot be identified", socket)
	}
	if uid != uint32(processUID()) && uid != 0 {
		return fmt.Errorf("socket %s is answered by another user", socket)
	}
	return nil
}

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
