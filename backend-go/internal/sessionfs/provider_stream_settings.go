package sessionfs

import "encoding/json"

const (
	defaultProviderStreamIdleTimeoutMS = 300_000
	minProviderStreamIdleTimeoutMS     = 1_000
	maxProviderStreamIdleTimeoutMS     = 3_600_000
)

var providerStreamKeys = []string{"idle_timeout_ms"}

// ProviderStreamSettings bounds a response stream that has stopped sending.
type ProviderStreamSettings struct {
	// IdleTimeoutMS is how long one provider call may go without a stream event before
	// the run fails. 0 turns the guard off.
	IdleTimeoutMS int64
}

// normalizeProviderStreamSettings reads the `provider_stream` node. Only an absent key
// takes a default: a null or misspelt value must not quietly pick one.
func normalizeProviderStreamSettings(raw json.RawMessage, settingPath string) (ProviderStreamSettings, error) {
	settings := ProviderStreamSettings{IdleTimeoutMS: defaultProviderStreamIdleTimeoutMS}
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": provider_stream")
	if err != nil {
		return ProviderStreamSettings{}, configError("%s: provider_stream must be an object", settingPath)
	}
	for key := range fields {
		if !containsString(providerStreamKeys, key) {
			return ProviderStreamSettings{}, unsupportedToolKey(settingPath+": provider_stream", key, providerStreamKeys, "")
		}
	}
	if value, exists := fields["idle_timeout_ms"]; exists {
		configured, ok := configSafeInteger(value)
		if !ok || configured < 0 || configured > maxProviderStreamIdleTimeoutMS || (configured > 0 && configured < minProviderStreamIdleTimeoutMS) {
			return ProviderStreamSettings{}, configError("%s: provider_stream.idle_timeout_ms must be 0 (off) or a safe integer between %d and %d", settingPath, minProviderStreamIdleTimeoutMS, maxProviderStreamIdleTimeoutMS)
		}
		settings.IdleTimeoutMS = configured
	}
	return settings, nil
}
