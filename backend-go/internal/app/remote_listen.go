package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// RemoteHandlers are the handlers for the listeners of one server. Main serves the UI routes
// and, when no socket is configured, the remote routes; Socket serves the remote routes alone
// on the unix socket.
type RemoteHandlers struct {
	Main   http.Handler
	Socket http.Handler
}

// HandlersFor builds them from the service. The remote routes have exactly one listener: the
// unix socket when listen_unix is set, which never exposes them on the network, and the plain
// TCP listener otherwise. That listener carries the bearer token of every remote in clear text,
// so it must sit behind a VPN or a loopback address (see LoadConfig).
func (s *Service) HandlersFor(config Config) RemoteHandlers {
	handlers := RemoteHandlers{Main: s.Handler}
	if s.Remote == nil {
		return handlers
	}
	if config.RemoteAPI.ListenUnix != "" {
		handlers.Socket = s.Remote
		return handlers
	}
	handlers.Main = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/remote/") || r.URL.Path == "/api/remote" {
			s.Remote.ServeHTTP(w, r)
			return
		}
		s.Handler.ServeHTTP(w, r)
	})
	return handlers
}

// ListenUnix opens the socket with the configured mode. A stale socket file is replaced; any
// other kind of file at the path is an error, and the socket is never left wider than asked.
func ListenUnix(path string, mode os.FileMode) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("server.remote_api.listen_unix: %s exists and is not a socket", path)
		}
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			return nil, fmt.Errorf("server.remote_api.listen_unix: %s is in use by another server", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("server.remote_api.listen_unix: remove the stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("server.remote_api.listen_unix: %s: %w", path, err)
	}
	// The socket is bound inside a fresh 0700 directory, set to its mode and only then renamed
	// into place, so it is never reachable by others and no process-wide umask is touched.
	staging, err := os.MkdirTemp(filepath.Dir(path), ".js")
	if err != nil {
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	defer os.Remove(staging)
	staged := filepath.Join(staging, "s")
	if len(staged) >= maxUnixSocketPath {
		return nil, fmt.Errorf("server.remote_api.listen_unix: %s is too long for a unix socket path; use a shorter path", path)
	}
	listener, err := netListen("unix", staged)
	if err != nil {
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	if err := os.Chmod(staged, mode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: chmod %s: %w", path, err)
	}
	// Link refuses an existing path, so a socket published by another listener in the meantime is
	// never replaced.
	if err := os.Link(staged, path); err != nil {
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	if err := os.Remove(staged); err != nil {
		os.Remove(path)
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	return &unixListener{Listener: listener, path: path, info: info}, nil
}

// maxUnixSocketPath is the smallest sun_path limit in use, macOS 104 bytes including the NUL;
// Linux allows 108.
const maxUnixSocketPath = 104

var netListen = net.Listen

// unixListener removes the socket file at its final path when it closes, but only if that path
// still holds the socket it published; the inner listener only knows the staging path.
type unixListener struct {
	net.Listener
	path string
	info os.FileInfo
}

func (l *unixListener) Close() error {
	err := l.Listener.Close()
	if info, statErr := os.Lstat(l.path); statErr == nil && os.SameFile(info, l.info) {
		_ = os.Remove(l.path)
	}
	return err
}
