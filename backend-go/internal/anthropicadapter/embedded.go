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
		if err := decodeConfig(trimmed, &config); err != nil {
			return nil, err
		}
	}
	if baseURL != "" {
		config.Upstream.BaseURL = baseURL
	}
	if err := config.finish(); err != nil {
		return nil, err
	}
	return &config, nil
}
