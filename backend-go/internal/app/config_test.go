package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

func TestLoadConfigAppliesRootSettingsAndEnvironmentOverrides(t *testing.T) {
	cwd := t.TempDir()
	setting := `{"session_roots":[{"id":"work","path":"sessions"}],"server":{"host":"127.0.0.1","port":8789,"max_body_bytes":8192,"run_state_retention_ms":0,"tool_stream_interval_ms":17,"sse_max_buffered_bytes":4096,"allowed_origins":["https://ui.example.test"]}}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir: cwd,
		Environment: map[string]string{
			"JAD_API_TOKEN": integrationAPIToken,
			"HOST":          "localhost",
			"PORT":          "0",
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Host != "localhost" || config.Port != 0 {
		t.Errorf("listener settings = %q:%d", config.Host, config.Port)
	}
	if len(config.Roots) != 1 || config.Roots[0].ID != "work" {
		t.Errorf("roots = %+v", config.Roots)
	}
	if config.HTTP.APIToken != integrationAPIToken || config.HTTP.MaxBodyBytes != 8192 || config.HTTP.RunStateRetention != 0 || !config.HTTP.AllowZeroRunStateRetention {
		t.Errorf("HTTP settings = %+v", config.HTTP)
	}
	if config.HTTP.MaxConcurrentRuns != 0 {
		t.Errorf("max concurrent runs = %d, want the default 0 (no cap)", config.HTTP.MaxConcurrentRuns)
	}
	if config.HTTP.ToolStreamInterval != 17*time.Millisecond {
		t.Errorf("tool stream interval = %s, want 17ms", config.HTTP.ToolStreamInterval)
	}
	if _, hasToken := config.Environment[apiTokenEnvironmentName]; hasToken {
		t.Error("API authentication token was retained in the session environment")
	}
	if len(config.HTTP.AllowedOrigins) != 1 || config.HTTP.AllowedOrigins[0] != "https://ui.example.test" {
		t.Errorf("allowed origins = %#v", config.HTTP.AllowedOrigins)
	}
}

func TestLoadConfigRejectsInvalidPortAndServiceRequiresToken(t *testing.T) {
	cwd := t.TempDir()
	if _, err := LoadConfig(sessionfs.LoadOptions{
		WorkingDir:  cwd,
		Environment: map[string]string{"PORT": "invalid"},
	}); err == nil {
		t.Fatal("LoadConfig(PORT=invalid) succeeded")
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadConfig(defaults) error = %v", err)
	}
	if _, err := NewService(config); err == nil {
		t.Fatal("NewService() without JAD_API_TOKEN succeeded")
	}
}

func TestLoadConfigRejectsLegacyAPITokenName(t *testing.T) {
	cwd := t.TempDir()
	_, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"AGENT_API_TOKEN": integrationAPIToken}})
	if err == nil || !strings.Contains(err.Error(), "JAD_API_TOKEN") || !strings.Contains(err.Error(), "AGENT_API_TOKEN") {
		t.Fatalf("LoadConfig(legacy token only) error = %v, want one naming both variables", err)
	}
	if _, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"AGENT_API_TOKEN": "old", "JAD_API_TOKEN": integrationAPIToken}}); err != nil {
		t.Fatalf("LoadConfig(both set) error = %v, want success", err)
	}
}

func TestLoadConfigPassesMaxConcurrentRunsToTheHTTPConfig(t *testing.T) {
	cwd := t.TempDir()
	setting := `{"session_roots":[{"id":"work","path":"sessions"}],"server":{"max_concurrent_runs":7}}`
	if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken}})
	if err != nil {
		t.Fatal(err)
	}
	if config.HTTP.MaxConcurrentRuns != 7 {
		t.Fatalf("HTTP.MaxConcurrentRuns = %d, want 7 from server.max_concurrent_runs", config.HTTP.MaxConcurrentRuns)
	}
}

// The remote API's bearer token travels in clear text over TCP, so its listener names one
// address: the default loopback, or the VPN address a peer reaches. Every spelling of "every
// interface" is refused, and so is anything that is not an address or a plain hostname; with
// listen_unix set nothing answers on TCP, so any host is fine.
func TestLoadConfigRefusesAWildcardHostForTheRemoteAPI(t *testing.T) {
	cwd := t.TempDir()
	writeSetting := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cwd, "setting.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	load := func(host string) (Config, error) {
		return LoadConfig(sessionfs.LoadOptions{WorkingDir: cwd, Environment: map[string]string{"JAD_API_TOKEN": integrationAPIToken, "HOST": host}})
	}
	refused := func(host, body string) {
		t.Helper()
		writeSetting(body)
		_, err := load(host)
		if err == nil || !strings.Contains(err.Error(), "server.host must name the one address") {
			t.Errorf("host %q: %v", host, err)
		}
	}
	accepted := func(host, body string) {
		t.Helper()
		writeSetting(body)
		config, err := load(host)
		if err != nil || config.Host != strings.TrimSpace(host) {
			t.Errorf("host %q: %+v %v", host, config.Host, err)
		}
	}
	tcpOnly := `{"session_roots":[],"server":{"remote_api":{"enabled":true,"tokens_file":"tokens.json"}}}`
	socketOnly := `{"session_roots":[],"server":{"remote_api":{"enabled":true,"tokens_file":"tokens.json","listen_unix":"/run/jad/remote.sock"}}}`
	wildcards := []string{"", "0.0.0.0", "::", "[::]", "0:0:0:0:0:0:0:0", "::0", "[::0]", "::ffff:0.0.0.0",
		"[::ffff:0.0.0.0]", "::%eth0", "0.0.0.0.", " [::] ", "0", "0.0", "0x0", "00", "0.0.0", "127.1", "0x7f.1", "1.2-3"}
	for _, host := range wildcards {
		refused(host, tcpOnly)
	}
	// Neither an address nor a plain hostname: the resolver would decide, so it is refused.
	for _, host := range []string{"*", "-bad", "trailing-", "a..b", "host:8787", "http://x"} {
		refused(host, tcpOnly)
	}
	for _, host := range []string{"127.0.0.1", "::1", "10.8.0.2", "localhost", "jad.vpn.example", "fe80::1%eth0", "host1", "1host", "10.example"} {
		accepted(host, tcpOnly)
	}
	// With listen_unix set the socket is the only listener, so any host is fine.
	for _, host := range wildcards {
		accepted(host, socketOnly)
	}
	// Without the remote API a wildcard host is fine: the UI listener is another matter.
	accepted("0.0.0.0", `{"session_roots":[],"server":{}}`)
}
