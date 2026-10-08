package openaiauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../../../sample/schemes/openai-auth.schema.json"

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	config, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if config.Issuer != DefaultIssuer || config.ClientID != DefaultClientID || config.LoginPort != DefaultLoginPort ||
		config.RedirectHost != DefaultRedirectHost || !config.KeepBackup || !config.OpenBrowser ||
		config.AuthorizeParams["codex_cli_simplified_flow"] != "true" {
		t.Fatalf("defaults wrong: %+v", config)
	}
	path, err := config.AuthFilePath()
	home, _ := os.UserHomeDir()
	if err != nil || path != filepath.Join(home, ".jad", "auth", "openai-codex.json") {
		t.Fatalf("default path %q err %v", path, err)
	}
}

func TestLoadConfigOverridesAndKeepsOtherDefaults(t *testing.T) {
	config, err := LoadConfig(writeConfig(t, `{"issuer":"https://id.example/","login_port":0,"keep_backup":false,
	  "authorize_params":{"x":"y"},"auth_file":"/tmp/a.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.LoginPort != 0 || config.KeepBackup || config.ClientID != DefaultClientID {
		t.Fatalf("override or default lost: %+v", config)
	}
	if len(config.AuthorizeParams) != 1 || config.AuthorizeParams["x"] != "y" {
		t.Fatalf("authorize_params must replace the defaults, got %v", config.AuthorizeParams)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]string{
		`{"bogus":1}`:                                 "unknown field",
		`{"schema_version":2}`:                        "schema_version",
		`{"auth_file":"relative/x.json"}`:             "auth_file",
		`{"auth_file":""}`:                            "auth_file",
		`{"issuer":"ftp://x"}`:                        "issuer",
		`{"issuer":"https://x/?a=1"}`:                 "issuer",
		`{"client_id":" "}`:                           "client_id",
		`{"scope":""}`:                                "scope",
		`{"redirect_host":"a/b"}`:                     "redirect_host",
		`{"redirect_host":"http://localhost"}`:        "redirect_host",
		`{"login_port":70000}`:                        "login_port",
		`{"login_port":-1}`:                           "login_port",
		`{"login_timeout_ms":0}`:                      "login_timeout_ms",
		`{"http_timeout_ms":0}`:                       "http_timeout_ms",
		`{"expiry_margin_ms":-1}`:                     "expiry_margin_ms",
		`{"authorize_params":{"state":"x"}}`:          "state",
		`{"authorize_params":{"redirect_uri":"x"}}`:   "redirect_uri",
		`{"authorize_params":{"code_challenge":"x"}}`: "code_challenge",
		`{"authorize_params":{"":"x"}}`:               "non-empty",
		`{"keep_backup":"yes"}`:                       "keep_backup",
		`{} {}`:                                       "trailing",
	}
	for body, want := range cases {
		_, err := LoadConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", body, want, err)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing config file must be an error")
	}
}

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	file, err := os.Open(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	document, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("openai-auth.schema.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("openai-auth.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// The schema documents the defaults; this keeps it and the Go constants equal.
func TestSchemaDefaultsMatchGo(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"schema_version":   float64(1),
		"auth_file":        DefaultAuthFile,
		"issuer":           DefaultIssuer,
		"client_id":        DefaultClientID,
		"scope":            DefaultScope,
		"redirect_host":    DefaultRedirectHost,
		"login_port":       float64(DefaultLoginPort),
		"login_timeout_ms": float64(DefaultLoginTimeoutMS),
		"open_browser":     DefaultOpenBrowser,
		"http_timeout_ms":  float64(DefaultHTTPTimeoutMS),
		"expiry_margin_ms": float64(DefaultExpiryMarginMS),
		"keep_backup":      DefaultKeepBackup,
	}
	for key, value := range want {
		if schema.Properties[key].Default != value {
			t.Errorf("schema default of %s is %v, Go has %v", key, schema.Properties[key].Default, value)
		}
	}
	params, _ := schema.Properties["authorize_params"].Default.(map[string]any)
	if len(params) != len(config.AuthorizeParams) {
		t.Fatalf("authorize_params default: schema %v, Go %v", params, config.AuthorizeParams)
	}
	for key, value := range config.AuthorizeParams {
		if params[key] != value {
			t.Errorf("authorize_params.%s: schema %v, Go %v", key, params[key], value)
		}
	}
	// Every config field must be a schema property, so a new key cannot skip the schema.
	var fields map[string]json.RawMessage
	encoded, _ := json.Marshal(config)
	_ = json.Unmarshal(encoded, &fields)
	for key := range fields {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("config key %s is missing from the schema", key)
		}
	}
}

func TestSchemaAcceptsAndRejects(t *testing.T) {
	schema := compileSchema(t)
	valid := `{"schema_version":1,"auth_file":"~/x.json","issuer":"https://a.b","login_port":0,"authorize_params":{"k":"v"},"keep_backup":false}`
	invalid := map[string]string{
		"unknown key":      `{"bogus":1}`,
		"relative file":    `{"auth_file":"x.json"}`,
		"bad issuer":       `{"issuer":"ftp://x"}`,
		"issuer query":     `{"issuer":"https://x/?a=1"}`,
		"negative port":    `{"login_port":-1}`,
		"zero timeout":     `{"login_timeout_ms":0}`,
		"reserved param":   `{"authorize_params":{"state":"x"}}`,
		"non-string param": `{"authorize_params":{"k":1}}`,
		"backup not bool":  `{"keep_backup":"yes"}`,
	}
	check := func(body string) error {
		var value any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		return schema.Validate(value)
	}
	if err := check(valid); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	for name, body := range invalid {
		if check(body) == nil {
			t.Errorf("%s: the schema accepted %s", name, body)
		}
	}
	// Whatever the schema accepts as valid must also load, and the reverse for these cases.
	if _, err := LoadConfig(writeConfig(t, valid)); err != nil {
		t.Errorf("schema-valid config does not load: %v", err)
	}
}

const samplePath = "../../../sample/openai-auth.json"

// The shipped sample must validate against the schema, load, and equal the defaults, so it cannot
// drift from them.
func TestSampleConfigIsValidAndEqualsTheDefaults(t *testing.T) {
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := compileSchema(t).Validate(value); err != nil {
		t.Fatalf("the sample does not match the schema: %v", err)
	}
	sample, err := LoadConfig(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	defaults, _ := LoadConfig("")
	got, _ := json.Marshal(sample)
	want, _ := json.Marshal(defaults)
	if string(got) != string(want) {
		t.Fatalf("the sample differs from the defaults:\n sample   %s\n defaults %s", got, want)
	}
}
