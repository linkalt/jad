package app

import (
	"context"
	"errors"
	"log/slog"

	"example.com/agent-harness/backend-go/internal/session"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools/native"
)

// fireRunHook runs the run_start or run_end commands of the session and writes each
// report to the server log. The reports never reach the model or messages.jsonl.
func (r boundRunner) fireRunHook(ctx context.Context, event native.RunEvent, onReport func(session.HookReport)) {
	fireSessionHook(ctx, r.resolver.logger, r.reference, r.config, event, onReport)
}

// fireSessionHook fills in the session fields of event, runs its commands, logs each report
// and hands it to onReport when that is set.
func fireSessionHook(ctx context.Context, logger *slog.Logger, reference string, config sessionfs.SessionConfig, event native.RunEvent, onReport func(session.HookReport)) {
	event.Session = reference
	event.Profile = config.Profile
	event.WorkspaceRoot = config.WorkingDir
	for _, report := range native.FireRunEvent(ctx, config.Hooks, config.Model.APIKeyEnv, event) {
		logger.Info("hook "+event.Event,
			slog.String("session", reference), slog.String("profile", config.Profile),
			slog.String("turn_id", event.TurnID), slog.String("trigger", event.Trigger),
			slog.String("report", report))
		if onReport != nil {
			onReport(session.HookReport{Event: event.Event, Trigger: event.Trigger, Report: report})
		}
	}
}

// runEndHook returns the function that reports the end of a run to the run_end commands.
// It runs on a context the stop request cannot cancel, so a stopped run still reports.
func (r boundRunner) runEndHook(ctx context.Context, onReport func(session.HookReport)) func(string, error) {
	if !r.config.Hooks.RunEventsActive() {
		return func(string, error) {}
	}
	detached := context.WithoutCancel(ctx)
	return func(turnID string, runErr error) {
		event := native.RunEvent{Event: native.HookEventRunEnd, TurnID: turnID, Outcome: native.RunOutcomeOK}
		switch {
		case errors.Is(runErr, context.Canceled):
			event.Outcome = native.RunOutcomeCancelled
		case runErr != nil:
			event.Outcome, event.Error = native.RunOutcomeError, runErr.Error()
		}
		r.fireRunHook(detached, event, onReport)
	}
}
