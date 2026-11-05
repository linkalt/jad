package app

import (
	"context"
	"encoding/json"
	"log/slog"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
)

// compactionSummarySettings returns the runner's compaction.summary settings, or nil when
// the summary is off.
func compactionSummarySettings(config sessionfs.SessionConfig) *session.CompactionSummarySettings {
	summary := config.Compaction.Summary
	if !summary.Enabled {
		return nil
	}
	return &session.CompactionSummarySettings{
		Prompt:          summary.Prompt,
		Header:          summary.Header,
		Role:            summary.Role,
		MaxInputBytes:   int(summary.MaxInputBytes),
		MaxItemBytes:    int(summary.MaxItemBytes),
		MaxOutputTokens: summary.MaxOutputTokens,
		TimeoutMS:       summary.TimeoutMS,
		OnFailure:       summary.OnFailure,
	}
}

// compactionSummaryLogger returns the OnSummaryError callback that logs a failed summary.
func compactionSummaryLogger(logger *slog.Logger, reference, profile, trigger string) func(error) {
	return func(err error) {
		if logger == nil {
			return
		}
		logger.Warn("compaction summary failed", "session", reference, "profile", profile, "trigger", trigger, "error", err)
	}
}

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

// manualCompactionSummarizer returns the summary callback of a manual compaction, or nil
// when compaction.summary is off. config must hold the model. The model client is built
// like a run's (credential, fallback, pricing); a credential that cannot be used is
// reported to the summary's failure rule, like a failed request.
func (r *Resolver) manualCompactionSummarizer(ctx context.Context, config sessionfs.SessionConfig, reference string) func([][]byte) (json.RawMessage, error) {
	settings := compactionSummarySettings(config)
	if settings == nil || r == nil {
		return nil
	}
	onError := compactionSummaryLogger(r.logger, reference, config.Profile, "manual")
	apiKey, err := r.providerAPIKey(config)
	if err != nil {
		return session.NewCompactionSummarizer(ctx, failingStreamer{err: err}, runnerModelSettings(config), *settings, onError)
	}
	streamer, _ := r.runStreamer(config, apiKey)
	return session.NewCompactionSummarizer(ctx, streamer, runnerModelSettings(config), *settings, onError)
}

// failingStreamer answers every request with err.
type failingStreamer struct{ err error }

func (s failingStreamer) Stream(context.Context, json.RawMessage, responses.EventHandler) error {
	return s.err
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
