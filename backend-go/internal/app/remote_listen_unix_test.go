//go:build unix

package app

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestListenUnixDoesNotChangeTheProcessUmask(t *testing.T) {
	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })
	dir, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	probe := filepath.Join(dir, "probe")
	original := netListen
	t.Cleanup(func() { netListen = original })
	var probeMode os.FileMode
	netListen = func(network, address string) (net.Listener, error) {
		if err := os.WriteFile(probe, nil, 0o666); err != nil {
			t.Error(err)
		} else if info, err := os.Stat(probe); err == nil {
			probeMode = info.Mode().Perm()
		}
		return original(network, address)
	}
	listener, err := ListenUnix(filepath.Join(dir, "r.sock"), 0o600)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	defer listener.Close()
	if probeMode != 0o644 {
		t.Fatalf("a file another goroutine creates while the socket is bound got mode %v; the umask must stay 022", probeMode)
	}
	if got := syscall.Umask(0o022); got != 0o022 {
		t.Fatalf("umask after ListenUnix: %o", got)
	}
}

func TestListenUnixLeavesNoStagingDirAndCloseRemovesTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "r.sock")
	listener, err := ListenUnix(path, 0o660)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 || entries[0].Name() != "r.sock" {
		t.Fatalf("directory after listen: %v", entries)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o660 {
		t.Fatalf("mode %v", info.Mode())
	}
	listener.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("closing must remove the socket: %v", entries)
	}
}
