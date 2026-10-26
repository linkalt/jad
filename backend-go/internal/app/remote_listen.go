package app

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// RemoteHandlers are the handlers for the listeners of one server. Main serves the UI routes
// and, only when it is HTTPS, the remote routes; Socket serves the remote routes alone on the
// unix socket.
type RemoteHandlers struct {
	Main   http.Handler
	Socket http.Handler
}

// HandlersFor builds them from the service. The plain listener never reaches the remote API:
// a request for /api/remote/ over plain http is a 404 whatever the token.
func (s *Service) HandlersFor(config Config) RemoteHandlers {
	handlers := RemoteHandlers{Main: s.Handler}
	if s.Remote == nil {
		return handlers
	}
	tlsMain := config.RemoteAPI.TLSCertFile != ""
	handlers.Main = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/remote/") || r.URL.Path == "/api/remote" {
			if !tlsMain || r.TLS == nil {
				http.NotFound(w, r)
				return
			}
			s.Remote.ServeHTTP(w, r)
			return
		}
		s.Handler.ServeHTTP(w, r)
	})
	if config.RemoteAPI.ListenUnix != "" {
		handlers.Socket = s.Remote
	}
	return handlers
}

// TLSConfig loads the certificate of server.remote_api; nil when the main listener stays plain.
func (c Config) TLSConfig() (*tls.Config, error) {
	if c.RemoteAPI.TLSCertFile == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(c.RemoteAPI.TLSCertFile, c.RemoteAPI.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("server.remote_api: load tls_cert_file and tls_key_file: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}, nil
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
	listener, err := netListen("unix", staged)
	if err != nil {
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	if err := os.Chmod(staged, mode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: chmod %s: %w", path, err)
	}
	if err := os.Rename(staged, path); err != nil {
		listener.Close()
		return nil, fmt.Errorf("server.remote_api.listen_unix: %w", err)
	}
	return &unixListener{Listener: listener, path: path}, nil
}

var netListen = net.Listen

// unixListener removes the socket file at its final path when it closes; the inner listener
// only knows the staging path.
type unixListener struct {
	net.Listener
	path string
}

func (l *unixListener) Close() error {
	err := l.Listener.Close()
	if info, statErr := os.Lstat(l.path); statErr == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(l.path)
	}
	return err
}
