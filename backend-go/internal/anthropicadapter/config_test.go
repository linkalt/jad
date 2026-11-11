package anthropicadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../../../sample/schemes/anthropic-adapter.schema.json"

// LoadConfig reads a standalone-format config file (testdata/*.json, written by writeConfig)
// the way ParseEmbeddedConfig reads a model file's anthropic object, minus the embedded
// restrictions (upstream.base_url is allowed) and the embedded cache default. An empty path
// returns the defaults. The standalone command that used it is gone; tests keep it as
// the fixture loader.
func LoadConfig(path string) (*Config, error) {
	config := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := decodeConfig(data, &config); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := config.finish(); err != nil {
		return nil, err
	}
	return &config, nil
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adapter.json")
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
	if config.DefaultMaxTokens != DefaultMaxTokens ||
		config.Upstream.BaseURL != DefaultUpstreamBaseURL || config.UnsupportedFields != UnsupportedIgnore {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if config.DefaultProfile.Thinking != ThinkingNone || config.DefaultProfile.EffortMap["high"] != "high" {
		t.Fatalf("unexpected default profile: %+v", config.DefaultProfile)
	}
}

// The schema documents the defaults; this keeps it and the Go constants equal.
func TestSchemaDefaultsMatchGo(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	defaultOf := func(key string) any {
		var property struct {
			Default any `json:"default"`
		}
		if err := json.Unmarshal(schema.Properties[key], &property); err != nil {
			t.Fatal(err)
		}
		return property.Default
	}
	config := DefaultConfig()
	checks := map[string]any{
		"default_max_tokens":              float64(config.DefaultMaxTokens),
		"max_request_bytes":               float64(config.MaxRequestBytes),
		"unsupported_fields":              config.UnsupportedFields,
		"missing_tool_result_text":        config.MissingToolResultText,
		"invalid_arguments_key":           config.InvalidArgumentsKey,
		"synthesize_missing_tool_results": config.SynthesizeMissingToolResults,
	}
	for key, want := range checks {
		if got := defaultOf(key); got != want {
			t.Errorf("schema default for %s is %v, Go default is %v", key, got, want)
		}
	}
	var stripSchema struct {
		Default []string `json:"default"`
	}
	if err := json.Unmarshal(schema.Properties["strip_schema_keywords"], &stripSchema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stripSchema.Default, config.StripSchemaKeywords) {
		t.Errorf("schema default for strip_schema_keywords is %v, Go default is %v", stripSchema.Default, config.StripSchemaKeywords)
	}
	var logSchema struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties["log"], &logSchema); err != nil {
		t.Fatal(err)
	}
	if logSchema.Properties["upstream_errors"].Default != config.Log.UpstreamErrors ||
		logSchema.Properties["body_bytes"].Default != float64(config.Log.BodyBytes) {
		t.Errorf("log schema defaults differ from Go: %+v", logSchema.Properties)
	}
	wantHeaders, _ := json.Marshal(config.Log.Headers)
	gotHeaders, _ := json.Marshal(logSchema.Properties["headers"].Default)
	if string(wantHeaders) != string(gotHeaders) {
		t.Errorf("log.headers schema default %s, Go default %s", gotHeaders, wantHeaders)
	}
	var cacheSchema struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties["cache"], &cacheSchema); err != nil {
		t.Fatal(err)
	}
	if cacheSchema.Properties["automatic"].Default != config.Cache.Automatic ||
		cacheSchema.Properties["tools"].Default != config.Cache.Tools ||
		cacheSchema.Properties["system"].Default != config.Cache.System ||
		cacheSchema.Properties["history"].Default != config.Cache.History ||
		cacheSchema.Properties["ttl"].Default != config.Cache.TTL {
		t.Errorf("cache schema defaults differ from Go: %+v", cacheSchema.Properties)
	}
	var upstream struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties["upstream"], &upstream); err != nil {
		t.Fatal(err)
	}
	var upstreamFull struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties["upstream"], &upstreamFull); err != nil {
		t.Fatal(err)
	}
	var retry struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(upstreamFull.Properties["stream_retry"], &retry); err != nil {
		t.Fatal(err)
	}
	if retry.Properties["max_retries"].Default != float64(config.Upstream.StreamRetry.MaxRetries) ||
		retry.Properties["delay_ms"].Default != float64(config.Upstream.StreamRetry.DelayMS) {
		t.Errorf("stream_retry schema defaults differ from Go: %+v", retry.Properties)
	}
	if upstream.Properties["base_url"].Default != config.Upstream.BaseURL ||
		upstream.Properties["anthropic_version"].Default != config.Upstream.AnthropicVersion ||
		upstream.Properties["auth_header"].Default != config.Upstream.AuthHeader ||
		upstream.Properties["credential_file"].Default != config.Upstream.CredentialFile ||
		upstream.Properties["system_prefix"].Default != config.Upstream.SystemPrefix ||
		upstream.Properties["expiry_margin_ms"].Default != float64(config.Upstream.ExpiryMarginMS) ||
		upstream.Properties["max_event_bytes"].Default != float64(config.Upstream.MaxEventBytes) ||
		upstream.Properties["request_timeout_ms"].Default != float64(config.Upstream.RequestTimeoutMS) {
		t.Errorf("upstream schema defaults differ from Go: %+v", upstream.Properties)
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
	if err := compiler.AddResource("anthropic-adapter.schema.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("anthropic-adapter.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestSchemaAcceptsAndRejects(t *testing.T) {
	schema := compileSchema(t)
	valid := `{"schema_version":1,"upstream":{"beta":["a-b"]},
	  "models":[{"match":"^claude-opus-5","thinking":"adaptive","display":"summarized","effort_map":{"max":"max"}}]}`
	invalid := map[string]string{
		"unknown key":       `{"bogus":1}`,
		"listen":            `{"listen":"127.0.0.1:8788"}`,
		"caller token":      `{"caller_token_env":"TOKEN"}`,
		"bad policy":        `{"unsupported_fields":"maybe"}`,
		"model w/o match":   `{"models":[{"thinking":"adaptive"}]}`,
		"low budget":        `{"default_profile":{"effort_budget":{"low":10}}}`,
		"beta with comma":   `{"upstream":{"beta":["a,b"]}}`,
		"bad thinking mode": `{"default_profile":{"thinking":"always"}}`,
		"retry too high":    `{"upstream":{"stream_retry":{"max_retries":11}}}`,
		"negative delay":    `{"upstream":{"stream_retry":{"delay_ms":-1}}}`,
		"empty error type":  `{"upstream":{"stream_retry":{"error_types":[""]}}}`,
	}
	check := func(text string) error {
		instance, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		return schema.Validate(instance)
	}
	if err := check(valid); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	for name, text := range invalid {
		if check(text) == nil {
			t.Errorf("%s: schema accepted %s", name, text)
		}
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		`{"bogus":1}`:                                           "unknown field",
		`{"schema_version":2}`:                                  "schema_version",
		`{"listen":"127.0.0.1:8788"}`:                           "unknown field",
		`{"caller_token_env":"TOKEN"}`:                          "unknown field",
		`{"upstream":{"base_url":"ftp://x"}}`:                   "base_url",
		`{"upstream":{"request_timeout_ms":0}}`:                 "request_timeout_ms",
		`{"upstream":{"max_event_bytes":0}}`:                    "max_event_bytes",
		`{"upstream":{"max_event_bytes":-5}}`:                   "max_event_bytes",
		`{"upstream":{"max_event_bytes":1.5}}`:                  "max_event_bytes",
		`{"upstream":{"beta":["a,b"]}}`:                         "beta",
		`{"upstream":{"auth_header":"basic"}}`:                  "auth_header",
		`{"upstream":{"credential_file":"relative.json"}}`:      "credential_file",
		`{"upstream":{"expiry_margin_ms":-1}}`:                  "expiry_margin_ms",
		`{"upstream":{"extra_headers":{"bad name":"x"}}}`:       "valid HTTP header name",
		`{"upstream":{"extra_headers":{"Authorization":"x"}}}`:  "cannot be overridden",
		`{"upstream":{"extra_headers":{"Anthropic-Beta":"x"}}}`: "cannot be overridden",
		`{"upstream":{"extra_headers":{"X-A":"a\nb"}}}`:         "line break",
		`{"upstream":{"auth_header":""}}`:                       "auth_header",
		`{"upstream":{"stream_retry":{"max_retries":-1}}}`:      "max_retries",
		`{"upstream":{"stream_retry":{"max_retries":11}}}`:      "max_retries",
		`{"upstream":{"stream_retry":{"delay_ms":-5}}}`:         "delay_ms",
		`{"upstream":{"stream_retry":{"error_types":[""]}}}`:    "error_types",
		`{"default_max_tokens":0}`:                              "default_max_tokens",
		`{"unsupported_fields":"maybe"}`:                        "unsupported_fields",
		`{"models":[{"thinking":"adaptive"}]}`:                  "match",
		`{"models":[{"match":"("}]}`:                            "regular expression",
		`{"models":[{"match":"x","thinking":"always"}]}`:        "thinking",
		`{"default_profile":{"effort_map":{"high":"huge"}}}`:    "effort_map",
		`{"default_profile":{"effort_budget":{"low":5}}}`:       "effort_budget",
		`{"default_profile":{"default_effort":"loud"}}`:         "default_effort",
		`{} {}`: "trailing",
	}
	for text, want := range cases {
		_, err := LoadConfig(writeConfig(t, text))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %s: error %v, want it to mention %q", text, err, want)
		}
	}
}

func TestProfileSelectionAndPartialProfile(t *testing.T) {
	config, err := LoadConfig(writeConfig(t, `{"models":[
	  {"match":"^claude-opus-5","thinking":"adaptive","effort_map":{"max":"xhigh"}},
	  {"match":"claude-haiku-4","thinking":"budget"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	opus := config.ProfileFor("claude-opus-5-5")
	if opus.Thinking != ThinkingAdaptive || opus.EffortMap["max"] != "xhigh" || opus.EffortMap["high"] != "high" {
		t.Fatalf("opus profile wrong, partial map not merged with defaults: %+v", opus)
	}
	if got := config.ProfileFor("claude-haiku-4-5"); got.Thinking != ThinkingBudget || got.EffortBudget["low"] != 2048 {
		t.Fatalf("haiku profile wrong: %+v", got)
	}
	if got := config.ProfileFor("something-else"); got != &config.DefaultProfile {
		t.Fatal("an unmatched model must use the default profile")
	}
}

// The profile schema documents structured_output's default and values; this keeps them
// equal to the Go constants.
func TestSchemaStructuredOutputMatchesTheConstants(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			Profile struct {
				Properties struct {
					StructuredOutput struct {
						Default string   `json:"default"`
						Enum    []string `json:"enum"`
					} `json:"structured_output"`
				} `json:"properties"`
			} `json:"profile"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	property := schema.Defs.Profile.Properties.StructuredOutput
	if property.Default != StructuredOutputUnsupported || len(property.Enum) != 2 || property.Enum[0] != StructuredOutputNative || property.Enum[1] != StructuredOutputUnsupported {
		t.Fatalf("schema structured_output = %+v", property)
	}
	if DefaultConfig().DefaultProfile.StructuredOutput != "" {
		t.Fatal("the default profile must leave structured_output to finish")
	}
	config, err := LoadConfig("")
	if err != nil || config.DefaultProfile.StructuredOutput != StructuredOutputUnsupported {
		t.Fatalf("loaded default = %v %v", config, err)
	}
}
