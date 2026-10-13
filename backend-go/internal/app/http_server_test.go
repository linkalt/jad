package app

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func startTimeoutServer(t *testing.T, config Config) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := config.NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// readsEOF reports whether the server closes conn within limit.
func readsEOF(conn net.Conn, limit time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	_, err := bufio.NewReader(conn).ReadByte()
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return err != nil
}

func TestNewHTTPServerAppliesConfiguredTimeouts(t *testing.T) {
	server := Config{ReadHeaderTimeout: 1500 * time.Millisecond, IdleTimeout: 250 * time.Millisecond}.NewHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout != 1500*time.Millisecond || server.IdleTimeout != 250*time.Millisecond {
		t.Errorf("timeouts = %v / %v", server.ReadHeaderTimeout, server.IdleTimeout)
	}
	if server.ReadTimeout != 0 || server.WriteTimeout != 0 {
		t.Errorf("ReadTimeout/WriteTimeout = %v / %v, must stay zero so SSE streams are not cut", server.ReadTimeout, server.WriteTimeout)
	}
}

func TestServerClosesConnectionThatNeverFinishesHeaders(t *testing.T) {
	address := startTimeoutServer(t, Config{ReadHeaderTimeout: 150 * time.Millisecond})
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	if !readsEOF(conn, 3*time.Second) {
		t.Fatal("server kept a partial-header connection open past read_header_timeout")
	}
}

func TestServerKeepsPartialHeaderConnectionWhenTimeoutDisabled(t *testing.T) {
	address := startTimeoutServer(t, Config{})
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	if readsEOF(conn, 600*time.Millisecond) {
		t.Fatal("server closed a partial-header connection although the timeout is disabled")
	}
}

func TestServerClosesIdleKeepAliveConnection(t *testing.T) {
	address := startTimeoutServer(t, Config{IdleTimeout: 150 * time.Millisecond})
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("expected the idle connection to close")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("server kept an idle keep-alive connection open past idle_timeout")
	}
}

func TestLoadConfigCarriesTimeoutsFromRootSettings(t *testing.T) {
	cwd := t.TempDir()
	setting := `{"session_roots":[{"path":"sessions"}],"server":{"read_header_timeout_ms":2000,"idle_timeout_ms":900}}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
	if err != nil {
		t.Fatal(err)
	}
	if config.ReadHeaderTimeout != 2*time.Second || config.IdleTimeout != 900*time.Millisecond {
		t.Errorf("config timeouts = %v / %v", config.ReadHeaderTimeout, config.IdleTimeout)
	}
}
