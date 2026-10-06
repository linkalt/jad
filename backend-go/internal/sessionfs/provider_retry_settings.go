package sessionfs

import "encoding/json"

// The defaults equal what both SDKs did before the setting existed (two retries, 0.5 s
// doubling to 8 s), so an absent key keeps the old behaviour. They and the bounds mirror
// TypeScript's DEFAULT_PROVIDER_RETRY_* and MAX_PROVIDER_RETRY_* constants.
const (
	defaultProviderRetryMaxRetries     = 2
	maxProviderRetryMaxRetries         = 10
	defaultProviderRetryInitialDelayMS = 500
	maxProviderRetryInitialDelayMS     = 60_000
	defaultProviderRetryMaxDelayMS     = 8_000
	maxProviderRetryMaxDelayMS         = 600_000
)

var providerRetryKeys = []string{"initial_delay_ms", "max_delay_ms", "max_retries"}

// ProviderRetrySettings controls how a failed request to the model provider is retried.
// It applies to the request that opens a response stream only: once events have arrived,
// partial output was already shown, so a failure after that point is never retried.
type ProviderRetrySettings struct {
	// MaxRetries is the number of retries after the first attempt; 0 turns retrying off.
	MaxRetries int64
	// InitialDelayMS is the wait before the first retry. Each later wait doubles.
	InitialDelayMS int64
	// MaxDelayMS is the longest wait between attempts. A Retry-After the provider sends
	// is used instead of the backoff, capped to this.
	MaxDelayMS int64
}

// normalizeProviderRetrySettings reads the `provider_retry` node. Only an absent key takes
// a default: a null or misspelt value must not quietly pick one.
func normalizeProviderRetrySettings(raw json.RawMessage, settingPath string) (ProviderRetrySettings, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		decoded, err := decodeConfigObject(raw, settingPath+": provider_retry")
		if err != nil {
			return ProviderRetrySettings{}, configError("%s: provider_retry must be an object", settingPath)
		}
		fields = decoded
	}
	for key := range fields {
		if !containsString(providerRetryKeys, key) {
			return ProviderRetrySettings{}, unsupportedToolKey(settingPath+": provider_retry", key, providerRetryKeys, "")
		}
	}
	bounded := func(name string, fallback, minimum, maximum int64) (int64, error) {
		value, exists := fields[name]
		if !exists {
			return fallback, nil
		}
		configured, ok := configSafeInteger(value)
		if !ok || configured < minimum || configured > maximum {
			return 0, configError("%s: provider_retry.%s must be a safe integer between %d and %d", settingPath, name, minimum, maximum)
		}
		return configured, nil
	}
	var settings ProviderRetrySettings
	var err error
	// The checks run in the order TypeScript runs them, so the same document fails with
	// the same message in both backends.
	if settings.InitialDelayMS, err = bounded("initial_delay_ms", defaultProviderRetryInitialDelayMS, 1, maxProviderRetryInitialDelayMS); err != nil {
		return ProviderRetrySettings{}, err
	}
	if settings.MaxDelayMS, err = bounded("max_delay_ms", defaultProviderRetryMaxDelayMS, 1, maxProviderRetryMaxDelayMS); err != nil {
		return ProviderRetrySettings{}, err
	}
	if settings.InitialDelayMS > settings.MaxDelayMS {
		return ProviderRetrySettings{}, configError("%s: provider_retry.initial_delay_ms must not exceed max_delay_ms", settingPath)
	}
	if settings.MaxRetries, err = bounded("max_retries", defaultProviderRetryMaxRetries, 0, maxProviderRetryMaxRetries); err != nil {
		return ProviderRetrySettings{}, err
	}
	return settings, nil
}
