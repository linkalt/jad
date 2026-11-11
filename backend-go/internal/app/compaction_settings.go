package app

import (
	"log/slog"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
)

// runnerModelSettings is the session.ModelSettings of a loaded model.
func runnerModelSettings(config sessionfs.SessionConfig) session.ModelSettings {
	return session.ModelSettings{
		Model:          config.Model.Name,
		Request:        config.Model.Request,
		HostedTools:    config.Model.HostedTools,
		PromptRole:     config.Model.PromptRole,
		PromptText:     config.Model.PromptText,
		PromptCacheKey: promptCacheKey(config.Model, config.MessagesPath),
	}
}

// overflowSettings returns the runner's compaction.overflow settings, or nil when the
// recovery is off (it is also off without compaction.auto_enabled, which the runner checks).
func overflowSettings(config sessionfs.SessionConfig, logger *slog.Logger, reference string) *session.OverflowSettings {
	overflow := config.Compaction.Overflow
	if !overflow.Enabled {
		return nil
	}
	return &session.OverflowSettings{
		MaxAttempts:        int(overflow.MaxAttempts),
		Patterns:           append([]string(nil), overflow.Patterns...),
		ToolOutputMaxBytes: int(overflow.ToolOutputMaxBytes),
		OnCompacted: func(attempt int, compacted bool, providerErr error) {
			if logger == nil {
				return
			}
			logger.Info("context overflow compaction", "session", reference, "profile", config.Profile, "attempt", attempt, "compacted", compacted, "provider_error", providerErr)
		},
		OnToolOutputsCut: func(attempt, maxBytes, cut int) {
			if logger == nil {
				return
			}
			logger.Info("context overflow tool outputs cut", "session", reference, "profile", config.Profile, "attempt", attempt, "max_bytes", maxBytes, "cut", cut)
		},
	}
}
