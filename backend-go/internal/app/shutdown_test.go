package app

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func startHangingServer(t *testing.T, config Config) (*http.Server, string, chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := config.NewHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		close(release)
		_ = server.Close()
	})
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached the handler")
	}
	return server, listener.Addr().String(), release
}

func TestShutdownHTTPServerForcesAHangingRequestAfterTheTimeout(t *testing.T) {
	config := Config{ShutdownTimeout: 200 * time.Millisecond}
	server, _, _ := startHangingServer(t, config)
	began := time.Now()
	forced, err := config.ShutdownHTTPServer(server, nil)
	if err != nil {
		t.Fatalf("error = %v, want nil: a forced close is not a failure", err)
	}
	if !forced {
		t.Fatal("forced = false, want true for a request that never ends")
	}
	if took := time.Since(began); took < 150*time.Millisecond || took > 3*time.Second {
		t.Fatalf("took %v, want about the 200ms timeout", took)
	}
}

func TestShutdownHTTPServerWithoutRequestsReturnsAtOnce(t *testing.T) {
	config := Config{ShutdownTimeout: 10 * time.Second}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := config.NewHTTPServer(http.NotFoundHandler())
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	began := time.Now()
	forced, err := config.ShutdownHTTPServer(server, nil)
	if err != nil || forced {
		t.Fatalf("forced = %v, error = %v, want a clean shutdown", forced, err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %v, an idle server must not wait for the timeout", took)
	}
}

func TestShutdownHTTPServerWaitsForARequestThatEndsInTime(t *testing.T) {
	config := Config{ShutdownTimeout: 5 * time.Second}
	server, _, release := startHangingServer(t, config)
	go func() {
		time.Sleep(100 * time.Millisecond)
		release <- struct{}{}
	}()
	forced, err := config.ShutdownHTTPServer(server, nil)
	if err != nil || forced {
		t.Fatalf("forced = %v, error = %v, want a clean shutdown", forced, err)
	}
}

func TestLoadConfigCarriesTheShutdownTimeout(t *testing.T) {
	load := func(server string) Config {
		cwd := t.TempDir()
		setting := `{"session_roots":[{"path":"sessions"}],"server":` + server + `}`
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
		config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	if got := load(`{}`).ShutdownTimeout; got != 10*time.Second {
		t.Errorf("default = %v, want 10s", got)
	}
	if got := load(`{"shutdown_timeout_ms":2500}`).ShutdownTimeout; got != 2500*time.Millisecond {
		t.Errorf("configured = %v, want 2.5s", got)
	}
	if got := load(`{"shutdown_timeout_ms":0}`).ShutdownTimeout; got != 0 {
		t.Errorf("zero = %v, want 0", got)
	}
}
