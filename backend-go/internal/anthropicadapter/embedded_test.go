package anthropicadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseEmbeddedConfigDefaultsAndValues(t *testing.T) {
	config, err := ParseEmbeddedConfig(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Upstream.BaseURL != DefaultUpstreamBaseURL || config.DefaultMaxTokens != DefaultMaxTokens || config.HoldsCredential() {
		t.Fatalf("defaults = %+v", config)
	}
	config, err = ParseEmbeddedConfig(json.RawMessage(`{"default_max_tokens":1000,"cache":{"system":true},"models":[{"match":"^claude-","thinking":"adaptive"}]}`), "http://gateway:8080")
	if err != nil {
		t.Fatal(err)
	}
	if config.Upstream.BaseURL != "http://gateway:8080" || config.DefaultMaxTokens != 1000 || !config.Cache.System || config.ProfileFor("claude-x").Thinking != ThinkingAdaptive {
		t.Fatalf("config = %+v", config)
	}
}

func TestParseEmbeddedConfigRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"not an object":     `[]`,
		"listen":            `{"listen":"127.0.0.1:1"}`,
		"caller token":      `{"caller_token_env":"X"}`,
		"upstream base_url": `{"upstream":{"base_url":"http://x"}}`,
		"unknown key":       `{"upstreams":{}}`,
		"invalid value":     `{"default_max_tokens":0}`,
	} {
		if _, err := ParseEmbeddedConfig(json.RawMessage(raw), ""); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := ParseEmbeddedConfig(nil, "ftp://x"); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("a base URL that is not http: %v", err)
	}
}
