package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func lspSettings(raw string) (*NativeLSPSettings, error) {
	return normalizeNativeLSPSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"lsp":` + raw + `}}`)}, "", "s")
}

func TestLSPSettingsDefaultsAndValues(t *testing.T) {
	got, err := lspSettings(`{"servers":{"go":{"command":["gopls"],"extensions":[".go"],"language_id":"go"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestTimeoutMs != 30000 || got.DiagnosticsWaitMs != 3000 || got.IdleShutdownMs != 600000 || got.MaxResults != 50 || got.MaxFileBytes != 2<<20 || len(got.Servers) != 1 || got.Servers[0].Name != "go" {
		t.Fatalf("defaults = %+v", got)
	}
	got, err = lspSettings(`{"request_timeout_ms":100,"diagnostics_wait_ms":0,"idle_shutdown_ms":1000,"max_results":7,"max_file_bytes":10,
		"servers":{"py":{"command":["pyright-langserver","--stdio"],"extensions":[".py",".PYI"],"language_id":"python","initialization_options":{"a":1},"env":{"VIRTUAL_ENV":"/v"}},
		"go":{"command":["gopls"],"extensions":[".go"],"language_id":"go"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestTimeoutMs != 100 || got.DiagnosticsWaitMs != 0 || got.MaxResults != 7 || got.Servers[0].Name != "go" || got.Servers[1].Name != "py" ||
		string(got.Servers[1].InitializationOptions) != `{"a":1}` || got.Servers[1].Env["VIRTUAL_ENV"] != "/v" {
		t.Fatalf("values = %+v", got)
	}
	if server := got.ServerFor("/x/stubs.pyi"); server == nil || server.Name != "py" {
		t.Fatalf("ServerFor(.pyi) = %+v", server)
	}
	if got.ServerFor("/x/a.rs") != nil {
		t.Fatal("ServerFor(.rs) found a server")
	}
	if none, err := normalizeNativeLSPSettings(map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{}}`)}, "", "s"); none != nil || err != nil {
		t.Fatalf("absent = %+v %v", none, err)
	}
}

func TestLSPSettingsRefusals(t *testing.T) {
	server := func(body string) string { return `{"servers":{"go":` + body + `}}` }
	for name, test := range map[string]struct{ raw, want string }{
		"no servers":        {`{}`, "servers must name one or more language servers"},
		"empty servers":     {`{"servers":{}}`, "servers must name one or more language servers"},
		"unknown key":       {`{"servers":{},"timeout":1}`, "tools.native.lsp.timeout is not supported"},
		"bad name":          {`{"servers":{"Go":{}}}`, `"Go" must be 1-64`},
		"server not object": {server(`1`), "servers.go must be an object"},
		"server extra key":  {server(`{"command":["gopls"],"extensions":[".go"],"language_id":"go","args":[]}`), "servers.go.args is not supported"},
		"no command":        {server(`{"extensions":[".go"],"language_id":"go"}`), "command must be a non-empty array"},
		"empty command":     {server(`{"command":[" "],"extensions":[".go"],"language_id":"go"}`), "command must be a non-empty array"},
		"no extensions":     {server(`{"command":["gopls"],"language_id":"go"}`), "extensions must list one or more"},
		"bad extension":     {server(`{"command":["gopls"],"extensions":["go"],"language_id":"go"}`), "extensions[0] must be a dot"},
		"no language":       {server(`{"command":["gopls"],"extensions":[".go"]}`), "language_id must be the LSP language identifier"},
		"options not obj":   {server(`{"command":["gopls"],"extensions":[".go"],"language_id":"go","initialization_options":[]}`), "initialization_options must be an object"},
		"bad env name":      {server(`{"command":["gopls"],"extensions":[".go"],"language_id":"go","env":{"A B":"x"}}`), `"A B" is not an environment variable name`},
		"env not strings":   {server(`{"command":["gopls"],"extensions":[".go"],"language_id":"go","env":{"A":1}}`), "env must be an object of strings"},
		"shared extension":  {`{"servers":{"a":{"command":["x"],"extensions":[".go"],"language_id":"go"},"b":{"command":["y"],"extensions":[".GO"],"language_id":"go"}}}`, "servers.a and b both claim .GO"},
		"timeout too low":   {`{"request_timeout_ms":99,"servers":{}}`, "request_timeout_ms must be a safe integer between 100 and 600000"},
		"idle too low":      {`{"idle_shutdown_ms":999,"servers":{}}`, "idle_shutdown_ms must be a safe integer between 1000"},
		"results zero":      {`{"max_results":0,"servers":{}}`, "max_results must be a safe integer between 1 and 1000"},
	} {
		if _, err := lspSettings(test.raw); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

func TestLSPSchemaAgreesWithTheLoader(t *testing.T) {
	schema := compileSchema(t, "setting.schema.json#/properties/tools/properties/native/properties/lsp")
	gopls := `"go":{"command":["gopls"],"extensions":[".go"],"language_id":"go"}`
	for _, test := range []struct {
		raw    string
		accept bool
	}{
		{`{"servers":{` + gopls + `}}`, true},
		{`{"servers":{` + gopls + `},"paths":{"allow":["/opt/src"]},"request_timeout_ms":100,"diagnostics_wait_ms":0,"idle_shutdown_ms":1000,"max_results":1000,"max_file_bytes":10485760}`, true},
		{`{"servers":{"py":{"command":["pyright-langserver","--stdio"],"extensions":[".py"],"language_id":"python","initialization_options":{},"env":{"A_1":"x"}}}}`, true},
		{`{}`, false},
		{`{"servers":{}}`, false},
		{`{"servers":{` + gopls + `},"max_file_bytes":10485761}`, false},
		{`{"servers":{` + gopls + `},"diagnostics_wait_ms":60001}`, false},
		{`{"servers":{` + gopls + `},"other":1}`, false},
		{`{"servers":{"Go":{"command":["gopls"],"extensions":[".go"],"language_id":"go"}}}`, false},
		{`{"servers":{"go":{"command":[],"extensions":[".go"],"language_id":"go"}}}`, false},
		{`{"servers":{"go":{"command":["gopls"],"extensions":["go"],"language_id":"go"}}}`, false},
		{`{"servers":{"go":{"command":["gopls"],"extensions":[".go"]}}}`, false},
		{`{"servers":{"go":{"command":["gopls"],"extensions":[".go"],"language_id":"go","env":{"1A":"x"}}}}`, false},
	} {
		if got := schemaAccepts(t, schema, test.raw); got != test.accept {
			t.Errorf("schema accepts %s = %v, want %v", test.raw, got, test.accept)
		}
		if _, err := lspSettings(test.raw); (err == nil) != test.accept {
			t.Errorf("loader accepts %s = %v (err %v), want %v", test.raw, err == nil, err, test.accept)
		}
	}
}
