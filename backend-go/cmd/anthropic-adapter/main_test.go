package main

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/anthropicadapter"
)

func TestWarnIfExposed(t *testing.T) {
	cases := []struct {
		addr string
		warn bool
	}{
		{"127.0.0.1:8788", false},
		{"[::1]:8788", false},
		{"0.0.0.0:8788", true},
		{"[::]:8788", true},
		{"192.168.1.5:8788", true},
	}
	for _, tc := range cases {
		addr, err := net.ResolveTCPAddr("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		warnIfExposed(slog.New(slog.NewTextHandler(&out, nil)), addr)
		if got := strings.Contains(out.String(), "non-loopback"); got != tc.warn {
			t.Errorf("%s: warned=%v, want %v (%q)", tc.addr, got, tc.warn, out.String())
		}
	}
}

func TestRequireCallerToken(t *testing.T) {
	t.Setenv("ADAPTER_TEST_TOKEN", strings.Repeat("t", 32))
	credential := filepath.Join(t.TempDir(), "claude-oauth.json")
	load := func(extra string) *anthropicadapter.Config {
		path := filepath.Join(t.TempDir(), "adapter.json")
		text := `{"upstream":{"credential_file":"` + credential + `"}` + extra + `}`
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		config, err := anthropicadapter.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	noCredential, err := anthropicadapter.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		config *anthropicadapter.Config
		addr   string
		refuse bool
	}{
		{"loopback without token", load(""), "127.0.0.1:8788", false},
		{"non-loopback without token", load(""), "0.0.0.0:8788", true},
		{"non-loopback with token", load(`,"caller_token_env":"ADAPTER_TEST_TOKEN"`), "0.0.0.0:8788", false},
		{"non-loopback forwarding the caller credential", noCredential, "0.0.0.0:8788", false},
	}
	for _, tc := range cases {
		addr, err := net.ResolveTCPAddr("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := requireCallerToken(tc.config, addr) != nil; got != tc.refuse {
			t.Errorf("%s: refused=%v, want %v", tc.name, got, tc.refuse)
		}
	}
}

func TestDisplayUpstreamURLDropsUserinfoAndQuery(t *testing.T) {
	got := displayUpstreamURL("https://user:secret@example.com/v1?key=abc#frag")
	if got != "https://example.com/v1" {
		t.Fatalf("displayUpstreamURL() = %q, want scheme, host and path only", got)
	}
}

func TestDisplayUpstreamURLFallsBackForAMalformedURL(t *testing.T) {
	got := displayUpstreamURL("http://[::1")
	if got != "<invalid upstream URL>" {
		t.Fatalf("displayUpstreamURL() = %q, want the invalid-URL fallback", got)
	}
}
