package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestMCPCommandFindsTheServerFileAndRemovesItsTokens(t *testing.T) {
	fixture := useFixture(t, layouttest.Spec{
		MCP: map[string]map[string]any{"remote/docs": {}, "local/files": {}},
		MCPConfig: map[string]map[string]any{
			"remote/docs": {"transport": map[string]any{"type": "streamable_http", "url": "https://mcp.example/mcp", "oauth": map[string]any{}}},
			"local/files": {"transport": map[string]any{"type": "stdio", "command": "files-mcp"}},
		},
	})
	tokenPath := filepath.Join(fixture.StateDir, ".mcp-oauth", "remote", "docs.json")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := dispatchCommand(args, commandStreams{stdin: strings.NewReader(""), stdout: &out, stderr: &out})
		return out.String(), err
	}

	if out, err := run("mcp", "logout", "remote/docs"); err != nil || out != "No tokens were stored at "+tokenPath+"\n" {
		t.Fatalf("logout without tokens = %q, %v", out, err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("mcp", "logout", "remote/docs"); err != nil || out != "Removed "+tokenPath+"\n" {
		t.Fatalf("logout = %q, %v", out, err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file after logout: %v", err)
	}

	for _, test := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"mcp"}, exitUsage, "usage: jad mcp login"},
		{[]string{"mcp", "status"}, exitUsage, "usage: jad mcp login"},
		{[]string{"mcp", "login"}, exitUsage, "name one MCP server file"},
		{[]string{"mcp", "login", "--timeout", "0s", "remote/docs"}, exitUsage, "--timeout must be positive"},
		{[]string{"mcp", "login", "remote/missing"}, exitFailed, "no MCP server file remote/missing.json"},
		{[]string{"mcp", "login", "--root", "nope", "remote/docs"}, exitFailed, `no session root "nope"`},
		{[]string{"mcp", "login", "local/files"}, exitFailed, "needs a streamable_http transport with transport.oauth"},
	} {
		_, err := run(test.args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != test.code || exit.err == nil || !strings.Contains(exit.err.Error(), test.want) {
			t.Errorf("%v: %v, want exit %d with %q", test.args, err, test.code, test.want)
		}
	}
	if out, err := run("mcp", "--help"); err != nil || !strings.Contains(out, "jad mcp logout") {
		t.Errorf("help = %q, %v", out, err)
	}
}

// A stray file whose name is not an identifier in the mcp folder does not stop the command for a
// server that has a valid name, and the stray one cannot be named.
func TestMCPCommandIgnoresMCPFilesWithBadNames(t *testing.T) {
	fixture := useFixture(t, layouttest.Spec{
		MCP:       map[string]map[string]any{"remote/docs": {}},
		MCPConfig: map[string]map[string]any{"remote/docs": {"transport": map[string]any{"type": "streamable_http", "url": "https://mcp.example/mcp", "oauth": map[string]any{}}}},
	})
	for _, name := range []string{"remote/docs (1).json", "loose.json"} {
		path := filepath.Join(fixture.GlobalDir, "mcp", filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	streams := commandStreams{stdin: strings.NewReader(""), stdout: &out, stderr: &out}
	if err := dispatchCommand([]string{"mcp", "logout", "remote/docs"}, streams); err != nil {
		t.Fatalf("logout with a stray file in the folder: %v\n%s", err, out.String())
	}
	err := dispatchCommand([]string{"mcp", "logout", "remote/docs (1)"}, streams)
	var exit *exitError
	if !errors.As(err, &exit) || exit.err == nil || !strings.Contains(exit.err.Error(), "remote/docs (1)") {
		t.Fatalf("logout of the stray name: %v, want a refusal", err)
	}
}
