//go:build unix

package app

import (
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestListenUnixRefusesAPathPublishedWhileItBinds(t *testing.T) {
	dir, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "r.sock")
	original := netListen
	t.Cleanup(func() { netListen = original })
	var first net.Listener
	var firstErr error
	netListen = func(network, address string) (net.Listener, error) {
		netListen = original
		first, firstErr = ListenUnix(path, 0o600)
		return original(network, address)
	}
	second, err := ListenUnix(path, 0o660)
	if first != nil {
		defer first.Close()
	}
	if firstErr != nil {
		t.Fatalf("the first listener must be published: %v", firstErr)
	}
	if err == nil {
		second.Close()
		t.Fatal("a second listen on a path published meanwhile must fail")
	}
	if info, _ := os.Stat(path); info == nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the first listener must keep its socket: %v", info)
	}
}

func TestListenUnixCloseLeavesAnotherListenersSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "r.sock")
	old, err := ListenUnix(path, 0o600)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	next, err := ListenUnix(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	old.Close()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("closing the old listener removed the socket of another listener: %v %v", info, err)
	}
}

func TestListenUnixRefusesAPathTooLongForItsStagingSocket(t *testing.T) {
	base, err := os.MkdirTemp("", "jad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := filepath.Join(base, strings.Repeat("d", 100-len(base)-len("/")-len("/r.sock")))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "r.sock")
	if len(path) != 100 {
		t.Fatalf("test path is %d bytes", len(path))
	}
	if _, err := ListenUnix(path, 0o600); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("a 100-byte socket path must be refused with a clear error: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused path must leave no staging dir or socket: %v", entries)
	}
	short := filepath.Join(base, "r.sock")
	listener, err := ListenUnix(short, 0o600)
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	listener.Close()
}
