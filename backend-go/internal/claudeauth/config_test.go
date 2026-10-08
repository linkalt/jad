package claudeauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	config, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if config.ClientID != DefaultClientID || config.TokenURL != DefaultTokenURL || config.LoginPort != 53692 ||
		config.AuthorizeParams["code"] != "true" {
		t.Fatalf("defaults wrong: %+v", config)
	}
	path, err := config.AuthFilePath()
	home, _ := os.UserHomeDir()
	if err != nil || path != filepath.Join(home, ".jad", "auth", "claude-oauth.json") {
		t.Fatalf("default path %q err %v", path, err)
	}
}

func TestLoadConfigOverridesAndKeepsOtherDefaults(t *testing.T) {
	config, err := LoadConfig(writeConfig(t, `{"token_url":"https://id.example/t","login_port":0,"keep_backup":false,
	  "authorize_params":{"x":"y"},"auth_file":"/tmp/a.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.LoginPort != 0 || config.KeepBackup || config.ClientID != DefaultClientID || config.TokenURL != "https://id.example/t" {
		t.Fatalf("override or default lost: %+v", config)
	}
	if len(config.AuthorizeParams) != 1 || config.AuthorizeParams["x"] != "y" {
		t.Fatalf("authorize_params must replace the defaults, got %v", config.AuthorizeParams)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]string{
		`{"bogus":1}`:                               "unknown field",
		`{"schema_version":2}`:                      "schema_version",
		`{"auth_file":"relative/x.json"}`:           "auth_file",
		`{"auth_file":""}`:                          "auth_file",
		`{"token_url":"ftp://x/y"}`:                 "token_url",
		`{"token_url":"https://x/y?a=b"}`:           "token_url",
		`{"authorize_url":"not a url"}`:             "authorize_url",
		`{"copy_code_uri":""}`:                      "copy_code_uri",
		`{"client_id":" "}`:                         "client_id",
		`{"scope":""}`:                              "scope",
		`{"redirect_host":"a b"}`:                   "redirect_host",
		`{"callback_path":"callback"}`:              "callback_path",
		`{"callback_path":"/cb?x=1"}`:               "callback_path",
		`{"login_port":70000}`:                      "login_port",
		`{"login_timeout_ms":0}`:                    "login_timeout_ms",
		`{"http_timeout_ms":99999999}`:              "http_timeout_ms",
		`{"expiry_margin_ms":-1}`:                   "expiry_margin_ms",
		`{"authorize_params":{"state":"x"}}`:        "authorize_params.state",
		`{"authorize_params":{"":"x"}}`:             "authorize_params keys",
		`{"schema_version":1} {"schema_version":1}`: "trailing data",
	}
	for text, want := range cases {
		_, err := LoadConfig(writeConfig(t, text))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %s: error %v, want containing %q", text, err, want)
		}
	}
}

// The sample file and the schema must state the same defaults as the code.
func TestSampleConfigAndSchemaMatchTheDefaults(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	sample, err := os.ReadFile(filepath.Join(root, "sample", "claude-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(filepath.Join(root, "sample", "claude-auth.json"))
	if err != nil {
		t.Fatalf("sample does not load: %v", err)
	}
	defaults, _ := LoadConfig("")
	if !reflect.DeepEqual(loaded, defaults) {
		t.Fatalf("sample differs from the defaults:\n sample %+v\n default %+v", loaded, defaults)
	}
	var sampleKeys map[string]any
	if err := json.Unmarshal(sample, &sampleKeys); err != nil {
		t.Fatal(err)
	}

	schemaText, err := os.ReadFile(filepath.Join(root, "sample", "schemes", "claude-auth.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
		AdditionalProperties bool `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schemaText, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties {
		t.Error("the schema must reject unknown keys")
	}
	var codeKeys []string
	configType := reflect.TypeOf(Config{})
	for index := 0; index < configType.NumField(); index++ {
		codeKeys = append(codeKeys, strings.Split(configType.Field(index).Tag.Get("json"), ",")[0])
	}
	for _, key := range codeKeys {
		property, ok := schema.Properties[key]
		if !ok {
			t.Errorf("schema has no property %q", key)
			continue
		}
		if _, ok := sampleKeys[key]; !ok {
			t.Errorf("sample has no key %q", key)
		}
		if key == "schema_version" {
			continue
		}
		// JSON numbers decode to float64 on both sides, so DeepEqual compares like with like.
		if want := sampleKeys[key]; !reflect.DeepEqual(property.Default, want) {
			t.Errorf("schema default for %q is %v, the sample (= code default) has %v", key, property.Default, want)
		}
	}
	if len(schema.Properties) != len(codeKeys) {
		t.Errorf("schema has %d properties, Config has %d keys", len(schema.Properties), len(codeKeys))
	}
}
