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
	// The listener keys of the removed standalone command are refused by name, not as
	// unknown fields, so a model file copied from an old adapter config says why.
	for _, key := range []string{"listen", "caller_token_env"} {
		_, err := ParseEmbeddedConfig(json.RawMessage(`{"`+key+`":"x"}`), "")
		if err == nil || !strings.HasPrefix(err.Error(), key+" is not used") || !strings.Contains(err.Error(), "no listener") {
			t.Errorf("%s: error %v", key, err)
		}
	}
	if _, err := ParseEmbeddedConfig(nil, "ftp://x"); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("a base URL that is not http: %v", err)
	}
}

// A model jad serves in process caches by default: tools, system and the last conversation
// block (not the state context after it). A cache object replaces the default as a whole.
func TestEmbeddedConfigCachesByDefault(t *testing.T) {
	for _, test := range []struct {
		raw, want string
	}{
		{``, "tool1,system1,msg2.0"},
		{`null`, "tool1,system1,msg2.0"},
		{`{"default_max_tokens":1000}`, "tool1,system1,msg2.0"},
		{`{"cache":{}}`, ""},
		{`{"cache":{"automatic":true}}`, "top"},
		{`{"cache":{"tools":true,"ttl":"1h"}}`, "tool1"},
	} {
		config, err := ParseEmbeddedConfig(json.RawMessage(test.raw), "")
		if err != nil {
			t.Fatalf("%s: %v", test.raw, err)
		}
		request := translateOK(t, config, cacheBody)
		if got := strings.Join(markedPaths(t, request), ","); got != test.want {
			t.Errorf("%q: marked %q, want %q", test.raw, got, test.want)
		}
	}
	// DefaultConfig itself keeps caching off.
	if DefaultConfig().Cache != (CacheConfig{}) {
		t.Fatalf("DefaultConfig().Cache = %+v", DefaultConfig().Cache)
	}
}
