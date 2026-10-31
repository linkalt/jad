//go:build unix

package remote

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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
