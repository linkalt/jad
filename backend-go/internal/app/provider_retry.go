package app

import (
	"log/slog"
	"time"

	"jad/backend-go/internal/openaiapi"
	"jad/backend-go/internal/sessionfs"
)

// providerRetryConfig turns the session's provider_retry setting into the client's retry
// configuration and logs each retry, like the TypeScript backend's [provider-retry] line.
func providerRetryConfig(settings sessionfs.ProviderRetrySettings, logger *slog.Logger) openaiapi.RetryConfig {
	return openaiapi.RetryConfig{
		MaxRetries:   int(settings.MaxRetries),
		InitialDelay: time.Duration(settings.InitialDelayMS) * time.Millisecond,
		MaxDelay:     time.Duration(settings.MaxDelayMS) * time.Millisecond,
		OnRetry: func(event openaiapi.RetryEvent) {
			if logger == nil {
				return
			}
			logger.Warn("provider request retry",
				"attempt", event.Attempt,
				"max_retries", event.MaxRetries,
				"status", event.Status,
				"wait_ms", event.Delay.Milliseconds(),
			)
		},
	}
}
