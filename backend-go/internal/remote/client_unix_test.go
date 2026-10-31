//go:build unix

package remote

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestClientUnixSocketOwnerAndDirectoryAreChecked(t *testing.T) {
	serve := func(socket string) *atomic.Int32 {
		var calls atomic.Int32
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Skip("unix sockets unavailable:", err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		})}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
		return &calls
	}
	call := func(socket string) error {
		entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"unix://`+socket+`","token_env":"JAD_REMOTE_T","retries":0}}`))
		if err != nil {
			t.Fatal(err)
		}
		return newClient(t, entry).Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil)
	}

	own := shortSocket(t)
	calls := serve(own)
	if err := call(own); err != nil || calls.Load() != 1 {
		t.Fatalf("a socket of this user in a private directory: %v", err)
	}

	original := processUID
	if original() == 0 {
		t.Skip("root owns every socket it makes, and root-owned sockets are trusted")
	}
	t.Cleanup(func() { processUID = original })
	processUID = func() int { return original() + 4242 }
	if err := call(own); err == nil || !strings.Contains(err.Error(), "owned by another user") || calls.Load() != 1 {
		t.Fatalf("a socket owned by someone else must not get the token: %v", err)
	}
	processUID = original

	open := shortSocket(t)
	openCalls := serve(open)
	if err := os.Chmod(filepath.Dir(open), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := call(open); err == nil || !strings.Contains(err.Error(), "other users can write") || openCalls.Load() != 0 {
		t.Fatalf("a world-writable directory without the sticky bit must be refused: %v", err)
	}
	if err := os.Chmod(filepath.Dir(open), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := call(open); err != nil || openCalls.Load() != 1 {
		t.Fatalf("a sticky shared directory is fine: %v", err)
	}

	// The test runs as one user and cannot make a peer owned by another, so the peer credential is
	// forced to a foreign uid. The socket path itself stays this user's and passes the pre-dial check.
	peered := shortSocket(t)
	peeredCalls := serve(peered)
	originalPeer := peerUID
	t.Cleanup(func() { peerUID = originalPeer })
	peerUID = func(*net.UnixConn) (uint32, error) { return uint32(original() + 4242), nil }
	if err := call(peered); err == nil || !strings.Contains(err.Error(), "answered by another user") || peeredCalls.Load() != 0 {
		t.Fatalf("a peer that is not this user must be refused after the dial: %v", err)
	}
	peerUID = originalPeer

	grouped := shortSocket(t)
	groupedCalls := serve(grouped)
	if err := os.Chmod(filepath.Dir(grouped), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := call(grouped); err == nil || !strings.Contains(err.Error(), "other users can write") || groupedCalls.Load() != 0 {
		t.Fatalf("a group-writable directory without the sticky bit must be refused: %v", err)
	}
}

// TestClientRefusesARealForeignPeerAfterTheDial runs the listener as another user, so the peer that
// answers is really foreign. The socket is then chowned to this user, so the pre-dial owner check
// passes and only the post-dial peer check can refuse it.
func TestClientRefusesARealForeignPeerAfterTheDial(t *testing.T) {
	if _, err := exec.LookPath("sudo"); err != nil {
		t.Skip("sudo is unavailable:", err)
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Skip("sudo needs a password, so no listener can run as another user:", err)
	}
	if _, err := user.Lookup("daemon"); err != nil {
		t.Skip("no daemon user to run the foreign listener:", err)
	}
	entry, err := ParseEntry("box", "/x/box.json", []byte(`{"kind":"remote","remote":{"url":"unix://`+shortSocket(t)+`","token_env":"JAD_REMOTE_T","timeout_ms":2000,"retries":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	socket := entry.Unix
	if err := os.Chmod(filepath.Dir(socket), 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	listener := exec.Command("sudo", "-n", "-u", "daemon", "socat", "UNIX-LISTEN:"+socket+",fork", "OPEN:/dev/null,rdonly")
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Process.Signal(syscall.SIGTERM)
		listener.Wait()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the foreign listener never bound its socket")
		}
	}
	if out, err := exec.Command("sudo", "-n", "chown", strconv.Itoa(os.Getuid()), socket).CombinedOutput(); err != nil {
		t.Fatalf("chown: %v: %s", err, out)
	}
	err = newClient(t, entry).Do(context.Background(), Request{Method: "GET", Path: "/x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "answered by another user") {
		t.Fatalf("a socket answered by another user must be refused after the dial: %v", err)
	}
}
