package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func TestNormalizeConfigDefaultsAndAppliesStdioEnvironmentPrecedence(t *testing.T) {
	cwd := t.TempDir()
	serverCWD := filepath.Join(cwd, "server")
	if err := os.Mkdir(serverCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	reference := sessionfs.MCPServerReference{ID: "fixture", ConfigPath: filepath.Join(cwd, "server.json"), Enabled: true}
	environment := map[string]string{"PATH": "/bin", "MCP_TOKEN": "secret-value", "MCP_SETTING": "from-parent"}
	config, err := normalizeConfig(json.RawMessage(`{
		"transport":{"type":"stdio","command":"fixture","args":null,"cwd":"server","env_from":{"MCP_AUTH":"MCP_TOKEN"},"env":{"MCP_SETTING":"from-config"}}
	}`), reference, cwd, environment)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if config.timeout != 30_000*time.Millisecond || config.maxResultBytes != 1_048_576 {
		t.Fatalf("default MCP limits = timeout %v, max bytes %d", config.timeout, config.maxResultBytes)
	}
	stdio := config.transport.stdio
	if stdio == nil || stdio.cwd != serverCWD || len(stdio.args) != 0 {
		t.Fatalf("normalized stdio config = %+v", stdio)
	}
	if stdio.env["MCP_AUTH"] != "secret-value" || stdio.env["MCP_SETTING"] != "from-config" || stdio.env["PATH"] != "/bin" {
		t.Fatalf("effective stdio environment = %#v", stdio.env)
	}
	if _, exists := stdio.env["MCP_TOKEN"]; exists {
		t.Fatalf("non-allowlisted environment secret was inherited: %#v", stdio.env)
	}
	changed, err := normalizeConfig(json.RawMessage(`{"transport":{"type":"stdio","command":"fixture","cwd":"server","env_from":{"MCP_AUTH":"MCP_TOKEN"}}}`), reference, cwd, map[string]string{"MCP_TOKEN": "rotated"})
	if err != nil {
		t.Fatalf("normalizeConfig(rotated secret) error = %v", err)
	}
	if changed.fingerprint == config.fingerprint {
		t.Fatal("fingerprint did not change when the referenced environment value changed")
	}
}

func TestDefaultEnvironmentMatchesNodeSDKAllowlist(t *testing.T) {
	unix := defaultEnvironment(map[string]string{
		"HOME": "/home/agent", "LOGNAME": "agent", "PATH": "/safe/bin", "SHELL": "()malicious", "TERM": "xterm", "USER": "agent", "MCP_SECRET": "do-not-inherit",
	}, "linux")
	for name, want := range map[string]string{"HOME": "/home/agent", "LOGNAME": "agent", "PATH": "/safe/bin", "TERM": "xterm", "USER": "agent"} {
		if unix[name] != want {
			t.Errorf("Unix default environment %s = %q, want %q", name, unix[name], want)
		}
	}
	if _, exists := unix["SHELL"]; exists {
		t.Fatal("function-valued SHELL should be filtered")
	}
	if _, exists := unix["MCP_SECRET"]; exists {
		t.Fatal("non-allowlisted secret should not be inherited")
	}

	windows := defaultEnvironment(map[string]string{"appdata": `C:\Users\agent\AppData`, "Path": `C:\safe\bin`, "USERNAME": "agent", "MCP_SECRET": "do-not-inherit"}, "windows")
	if windows["APPDATA"] != `C:\Users\agent\AppData` || windows["PATH"] != `C:\safe\bin` || windows["USERNAME"] != "agent" {
		t.Fatalf("Windows default environment = %#v", windows)
	}
	if _, exists := windows["MCP_SECRET"]; exists {
		t.Fatal("Windows default environment inherited a non-allowlisted secret")
	}
}

func TestNormalizeConfigStreamableHTTPAndRejectsUnsafeURLs(t *testing.T) {
	reference := sessionfs.MCPServerReference{ID: "remote", ConfigPath: "/session/remote.json"}
	config, err := normalizeConfig(json.RawMessage(`{"transport":{"type":"streamable_http","url":"https://mcp.example.test/mcp","headers_from":{"Authorization":"MCP_TOKEN"},"headers_from_extra":true},"timeout_ms":1200,"max_result_bytes":2048}`), reference, t.TempDir(), map[string]string{"MCP_TOKEN": "Bearer token"})
	if err != nil {
		t.Fatalf("normalizeConfig(streamable HTTP) error = %v", err)
	}
	if config.timeout != 1200*time.Millisecond || config.maxResultBytes != 2048 || config.transport.http == nil || config.transport.http.headers["Authorization"] != "Bearer token" {
		t.Fatalf("normalized HTTP config = %+v", config)
	}
	for _, endpoint := range []string{"file:///tmp/mcp", "https://user:pass@mcp.example.test/mcp", "https://mcp.example.test/mcp#fragment"} {
		raw, _ := json.Marshal(map[string]any{"transport": map[string]any{"type": "streamable_http", "url": endpoint}})
		if _, err := normalizeConfig(raw, reference, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "transport.url") {
			t.Errorf("normalizeConfig(%q) error = %v, want URL validation error", endpoint, err)
		}
	}
}

func TestNormalizeConfigRejectsMissingSecretsAndInvalidLimits(t *testing.T) {
	reference := sessionfs.MCPServerReference{ID: "remote", ConfigPath: "/session/remote.json"}
	cases := []struct {
		name string
		raw  string
		env  map[string]string
		want string
	}{
		{name: "missing header secret", raw: `{"transport":{"type":"streamable_http","url":"https://mcp.example.test/mcp","headers_from":{"Authorization":"MCP_TOKEN"}}}`, want: "MCP_TOKEN"},
		{name: "timeout below minimum", raw: `{"transport":{"type":"stdio","command":"x"},"timeout_ms":0}`, want: "timeout_ms"},
		{name: "timeout above maximum", raw: `{"transport":{"type":"stdio","command":"x"},"timeout_ms":600001}`, want: "timeout_ms"},
		{name: "result limit below minimum", raw: `{"transport":{"type":"stdio","command":"x"},"max_result_bytes":511}`, want: "max_result_bytes"},
		{name: "duplicate stdio environment source", raw: `{"transport":{"type":"stdio","command":"x","env":{"TOKEN":"value"},"env_from":{"TOKEN":"MCP_TOKEN"}}}`, env: map[string]string{"MCP_TOKEN": "secret"}, want: "must not set the same variable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeConfig(json.RawMessage(test.raw), reference, t.TempDir(), test.env)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("normalizeConfig() error = %v, want substring %q", err, test.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("configuration error leaked a secret: %v", err)
			}
		})
	}
}

func TestMCPInputSchemaSupportsDraft04Through202012(t *testing.T) {
	cases := []struct {
		name   string
		draft  string
		schema string
	}{
		{name: "draft04", draft: "http://json-schema.org/draft-04/schema#", schema: `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object","properties":{"count":{"type":"number","minimum":0,"exclusiveMinimum":true}},"required":["count"]}`},
		{name: "draft06", draft: "http://json-schema.org/draft-06/schema#", schema: `{"$schema":"http://json-schema.org/draft-06/schema#","type":"object","properties":{"count":{"type":"number","exclusiveMinimum":0}},"required":["count"]}`},
		{name: "draft07", draft: "http://json-schema.org/draft-07/schema#", schema: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"count":{"type":"number","exclusiveMinimum":0}},"required":["count"]}`},
		{name: "draft2019", draft: "https://json-schema.org/draft/2019-09/schema", schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object","properties":{"count":{"type":"number","exclusiveMinimum":0}},"required":["count"]}`},
		{name: "draft2020", draft: "https://json-schema.org/draft/2020-12/schema", schema: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"count":{"type":"number","exclusiveMinimum":0}},"required":["count"]}`},
		{name: "defaultDraft07", schema: `{"type":"object","properties":{"count":{"type":"number","exclusiveMinimum":0}},"required":["count"]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			validator, err := tools.CompileSchema([]byte(test.schema))
			if err != nil {
				t.Fatalf("CompileSchema(%s) error = %v", test.draft, err)
			}
			issues, err := validator.Validate([]byte(`{"count":0}`))
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if len(issues) == 0 {
				t.Fatalf("schema %s accepted a value equal to its exclusive minimum", test.draft)
			}
		})
	}
}
