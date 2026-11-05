package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	if config.maxTools != defaultMaxTools || config.maxSchemaBytes != defaultMaxSchemaBytes || config.maxDescriptionBytes != defaultMaxDescriptionBytes {
		t.Fatalf("default MCP discovery limits = %d tools, %d schema bytes, %d description bytes", config.maxTools, config.maxSchemaBytes, config.maxDescriptionBytes)
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
		{name: "tool limit zero", raw: `{"transport":{"type":"stdio","command":"x"},"max_tools":0}`, want: "max_tools must be an integer"},
		{name: "tool limit negative", raw: `{"transport":{"type":"stdio","command":"x"},"max_tools":-1}`, want: "max_tools must be an integer"},
		{name: "tool limit not an integer", raw: `{"transport":{"type":"stdio","command":"x"},"max_tools":2.5}`, want: "max_tools must be an integer"},
		{name: "tool limit above maximum", raw: `{"transport":{"type":"stdio","command":"x"},"max_tools":2049}`, want: "max_tools must be an integer"},
		{name: "schema limit below minimum", raw: `{"transport":{"type":"stdio","command":"x"},"max_schema_bytes":511}`, want: "max_schema_bytes must be an integer"},
		{name: "schema limit zero", raw: `{"transport":{"type":"stdio","command":"x"},"max_schema_bytes":0}`, want: "max_schema_bytes must be an integer"},
		{name: "schema limit not an integer", raw: `{"transport":{"type":"stdio","command":"x"},"max_schema_bytes":512.5}`, want: "max_schema_bytes must be an integer"},
		{name: "schema limit above maximum", raw: `{"transport":{"type":"stdio","command":"x"},"max_schema_bytes":1048577}`, want: "max_schema_bytes must be an integer"},
		{name: "description limit zero", raw: `{"transport":{"type":"stdio","command":"x"},"max_description_bytes":0}`, want: "max_description_bytes must be an integer"},
		{name: "resource events zero", raw: `{"transport":{"type":"stdio","command":"x"},"resource_events_max":0}`, want: "resource_events_max must be an integer"},
		{name: "resource events too many", raw: `{"transport":{"type":"stdio","command":"x"},"resource_events_max":10001}`, want: "resource_events_max must be an integer"},
		{name: "description limit negative", raw: `{"transport":{"type":"stdio","command":"x"},"max_description_bytes":-4}`, want: "max_description_bytes must be an integer"},
		{name: "description limit not an integer", raw: `{"transport":{"type":"stdio","command":"x"},"max_description_bytes":64.25}`, want: "max_description_bytes must be an integer"},
		{name: "description limit above maximum", raw: `{"transport":{"type":"stdio","command":"x"},"max_description_bytes":65537}`, want: "max_description_bytes must be an integer"},
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

// Each draft is compiled and validated through this repository's CompileSchema: how a
// draft treats its own keywords is the dependency's business (jsonschema/v6 tests that
// upstream, and a test here that bypassed CompileSchema only re-tested it), while what
// this package must keep honest is that a server's declared draft reaches it.
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

// The schema in sample/schemes is the user-facing contract of an MCP server file: its
// keys, defaults and bounds must be the ones the loader enforces.
func TestMCPConfigSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../../sample/schemes/mcp-server.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Type    string  `json:"type"`
			Minimum float64 `json:"minimum"`
			Maximum float64 `json:"maximum"`
			Default float64 `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	limits := []struct {
		key              string
		minimum, maximum int64
		defaultValue     int64
	}{
		{key: "timeout_ms", minimum: 1, maximum: maxTimeoutMS, defaultValue: defaultTimeoutMS},
		{key: "max_result_bytes", minimum: minMaxResultBytes, maximum: maxMaxResultBytes, defaultValue: defaultMaxResultBytes},
		{key: "max_tools", minimum: minMaxTools, maximum: maxMaxTools, defaultValue: defaultMaxTools},
		{key: "max_schema_bytes", minimum: minMaxSchemaBytes, maximum: maxMaxSchemaBytes, defaultValue: defaultMaxSchemaBytes},
		{key: "max_description_bytes", minimum: minMaxDescriptionBytes, maximum: maxMaxDescriptionBytes, defaultValue: defaultMaxDescriptionBytes},
		{key: "resource_events_max", minimum: minResourceEventsMax, maximum: maxResourceEventsMax, defaultValue: defaultResourceEventsMax},
	}
	for _, limit := range limits {
		property, exists := schema.Properties[limit.key]
		if !exists {
			t.Errorf("mcp-server.schema.json does not document %s", limit.key)
			continue
		}
		if property.Type != "integer" || property.Minimum != float64(limit.minimum) || property.Maximum != float64(limit.maximum) || property.Default != float64(limit.defaultValue) {
			t.Errorf("%s = type %q min %v max %v default %v; the loader uses min %d max %d default %d",
				limit.key, property.Type, property.Minimum, property.Maximum, property.Default, limit.minimum, limit.maximum, limit.defaultValue)
		}
	}
	documented := []string{"name", "schema_version", "transport", "timeout_ms", "max_result_bytes", "max_tools", "max_schema_bytes", "max_description_bytes", "resource_events_max"}
	keys := make([]string, 0, len(schema.Properties))
	for key := range schema.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sort.Strings(documented)
	if strings.Join(keys, ",") != strings.Join(documented, ",") {
		t.Errorf("mcp-server.schema.json properties = %v, want %v", keys, documented)
	}
}

func TestNormalizeOAuthDefaultsAndRefusals(t *testing.T) {
	path := "/mcp/remote/docs.json"
	http := func(oauth string) string {
		return `{"transport":{"type":"streamable_http","url":"https://mcp.example/mcp","oauth":` + oauth + `}}`
	}
	config, err := normalizeConfig(json.RawMessage(http(`{}`)), sessionfs.MCPServerReference{ConfigPath: path}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	oauth := config.transport.http.oauth
	if oauth.grant != "authorization_code" || oauth.redirectURL != defaultOAuthRedirectURL || oauth.refreshMargin != defaultOAuthRefreshMarginMS*time.Millisecond || oauth.clientID != "" {
		t.Fatalf("defaults = %+v", oauth)
	}
	withSecret, err := normalizeConfig(json.RawMessage(http(`{"client_id":"c","client_secret_from":"S"}`)), sessionfs.MCPServerReference{ConfigPath: path}, "", map[string]string{"S": "one"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := normalizeConfig(json.RawMessage(http(`{"client_id":"c","client_secret_from":"S"}`)), sessionfs.MCPServerReference{ConfigPath: path}, "", map[string]string{"S": "two"})
	if err != nil {
		t.Fatal(err)
	}
	if withSecret.fingerprint == other.fingerprint || withSecret.fingerprint == config.fingerprint {
		t.Fatal("the OAuth settings and secret do not change the connection fingerprint")
	}
	if err := ValidateServerFile(path, []byte(http(`{"grant":"client_credentials","client_id":"c","client_secret_from":"S"}`))); err != nil {
		t.Fatalf("a file check refused client_credentials whose secret is in the environment: %v", err)
	}
	for _, test := range []struct{ oauth, want string }{
		{`[]`, "transport.oauth must be an object"},
		{`{"grants":"x"}`, "transport.oauth.grants is not supported"},
		{`{"grant":"password"}`, "grant must be authorization_code or client_credentials"},
		{`{"client_id":""}`, "client_id: must be a non-empty string"},
		{`{"client_secret_from":"S"}`, "client_secret_from needs transport.oauth.client_id"},
		{`{"client_id":"c","client_secret_from":"not-a-name"}`, "client_secret_from must be an environment variable name"},
		{`{"client_id":"c","client_secret_from":"MISSING"}`, "MISSING required by transport.oauth.client_secret_from is not set"},
		{`{"grant":"client_credentials","client_id":"c"}`, "client_credentials needs client_id and client_secret_from"},
		{`{"grant":"client_credentials","client_id":"c","client_secret_from":"S","scopes":["a"]}`, "scopes applies only to grant authorization_code"},
		{`{"scopes":[]}`, "scopes must list 1 to 64 scopes"},
		{`{"scopes":["a b"]}`, "scope tokens without spaces"},
		{`{"scopes":["a","a"]}`, "lists a twice"},
		{`{"redirect_url":"http://example.com:80/cb"}`, "must be https, or http on 127.0.0.1"},
		{`{"redirect_url":"http://127.0.0.1/cb"}`, "must name a port"},
		{`{"redirect_url":"https://example.com/cb?x=1"}`, "without credentials, query or fragment"},
		{`{"redirect_url":"ftp://example.com/cb"}`, "without credentials, query or fragment"},
		{`{"refresh_margin_ms":-1}`, "refresh_margin_ms must be an integer from 0 to 3600000"},
		{`{"refresh_margin_ms":3600001}`, "refresh_margin_ms must be an integer from 0 to 3600000"},
	} {
		_, err := normalizeConfig(json.RawMessage(http(test.oauth)), sessionfs.MCPServerReference{ConfigPath: path}, "", map[string]string{"S": "secret"})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("oauth %s: error = %v, want %q", test.oauth, err, test.want)
		}
	}
	both := `{"transport":{"type":"streamable_http","url":"https://mcp.example/mcp","headers_from":{"authorization":"TOKEN"},"oauth":{}}}`
	if _, err := normalizeConfig(json.RawMessage(both), sessionfs.MCPServerReference{ConfigPath: path}, "", map[string]string{"TOKEN": "x"}); err == nil || !strings.Contains(err.Error(), "must not both be set") {
		t.Errorf("oauth with an Authorization header: %v", err)
	}
}

// The schema's transport.oauth accepts what the loader accepts, for every rule a schema can
// state (the loopback rule of redirect_url is the loader's alone).
func TestMCPConfigSchemaOAuthMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../../sample/schemes/mcp-server.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	validator, err := tools.CompileSchema(data)
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"S": "secret"}
	for _, oauth := range []string{
		`{}`,
		`{"grant":"authorization_code","client_id":"c","client_secret_from":"S","scopes":["a","b:c"],"redirect_url":"https://example.com/cb","refresh_margin_ms":0}`,
		`{"grant":"client_credentials","client_id":"c","client_secret_from":"S"}`,
		`{"refresh_margin_ms":3600000}`,
		`{"grant":"password"}`,
		`{"grants":"x"}`,
		`{"client_id":""}`,
		`{"client_secret_from":"S"}`,
		`{"client_id":"c","client_secret_from":"not-a-name"}`,
		`{"grant":"client_credentials","client_id":"c"}`,
		`{"grant":"client_credentials","client_id":"c","client_secret_from":"S","scopes":["a"]}`,
		`{"grant":"client_credentials","client_id":"c","client_secret_from":"S","refresh_margin_ms":1}`,
		`{"scopes":[]}`,
		`{"scopes":["a b"]}`,
		`{"scopes":["a","a"]}`,
		`{"refresh_margin_ms":-1}`,
		`{"refresh_margin_ms":3600001}`,
		`{"refresh_margin_ms":1.5}`,
	} {
		raw := `{"transport":{"type":"streamable_http","url":"https://mcp.example/mcp","oauth":` + oauth + `}}`
		issues, err := validator.Validate([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		_, loadErr := normalizeConfig(json.RawMessage(raw), sessionfs.MCPServerReference{ConfigPath: "x.json"}, "", environment)
		if schemaOK, loaderOK := len(issues) == 0, loadErr == nil; schemaOK != loaderOK {
			t.Errorf("oauth %s: schema accepts %v, loader accepts %v (%v)", oauth, schemaOK, loaderOK, loadErr)
		}
	}
}
