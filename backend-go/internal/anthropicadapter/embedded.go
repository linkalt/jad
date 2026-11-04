package anthropicadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// embeddedRefusedKeys have no meaning when jad serves the adapter in process: there is
// no listener, so no address and no caller token.
var embeddedRefusedKeys = []string{"listen", "caller_token_env"}

// EmbeddedCacheDefault is the prompt caching of a model jad serves in process when its file
// sets no cache: breakpoints on the tools, the system prompt and the last conversation
// block, with the 5-minute lifetime. An agent sends the same tools, instructions and
// growing history every round, seconds apart, so the prefix is read back from the cache
// far more often than it is written. (The standalone adapter keeps caching off: its
// callers are unknown.) The last-history breakpoint, not the automatic one, because jad
// inserts its state context (task list, memory, processes) as messages after conversation
// items; the adapter folds them into text it never marks, so the breakpoint stays on the
// conversation and a changed state block does not move it.
func EmbeddedCacheDefault() CacheConfig {
	return CacheConfig{Tools: true, System: true, History: true}
}

// ParseEmbeddedConfig reads the anthropic object of a model file, for a model jad serves
// through the adapter in its own process. The object takes the keys of the adapter's own
// config file, except listen and caller_token_env (refused: nothing listens) and
// upstream.base_url, which is the model file's base_url. Absent or null means the
// defaults. An empty baseURL keeps the default upstream.
func ParseEmbeddedConfig(raw json.RawMessage, baseURL string) (*Config, error) {
	config := DefaultConfig()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &keys); err != nil || keys == nil {
			return nil, errors.New("must be a JSON object")
		}
		for _, name := range embeddedRefusedKeys {
			if _, ok := keys[name]; ok {
				return nil, fmt.Errorf("%s is not used: jad runs the adapter in process, with no listener", name)
			}
		}
		if upstream, ok := keys["upstream"]; ok {
			var upstreamKeys map[string]json.RawMessage
			if json.Unmarshal(upstream, &upstreamKeys) == nil {
				if _, ok := upstreamKeys["base_url"]; ok {
					return nil, errors.New("upstream.base_url is not used: set base_url in the model file")
				}
			}
		}
		// The embedded default caches; a cache object in the file replaces that default
		// as a whole, so {} turns caching off and one switch does not bring the others.
		if _, ok := keys["cache"]; !ok {
			config.Cache = EmbeddedCacheDefault()
		}
		if err := decodeConfig(trimmed, &config); err != nil {
			return nil, err
		}
	} else {
		config.Cache = EmbeddedCacheDefault()
	}
	if baseURL != "" {
		config.Upstream.BaseURL = baseURL
	}
	if err := config.finish(); err != nil {
		return nil, err
	}
	return &config, nil
}
