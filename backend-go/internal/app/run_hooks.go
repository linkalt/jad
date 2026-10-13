package app

import (
	"context"
	"errors"
	"log/slog"

	"example.com/agent-harness/backend-go/internal/tools/native"
)

// fireRunHook runs the run_start or run_end commands of the session and writes each
// report to the server log. The reports never reach the model or messages.jsonl.
func (r boundRunner) fireRunHook(ctx context.Context, event native.RunEvent) {
	event.Session = r.reference
	event.Profile = r.config.Profile
	event.WorkspaceRoot = r.config.WorkingDir
	for _, report := range native.FireRunEvent(ctx, r.config.Hooks, r.config.Model.APIKeyEnv, event) {
		r.resolver.logger.Info("hook "+event.Event,
			slog.String("session", r.reference), slog.String("profile", r.config.Profile),
			slog.String("turn_id", event.TurnID), slog.String("report", report))
	}
}

// runEndHook returns the function that reports the end of a run to the run_end commands.
// It runs on a context the stop request cannot cancel, so a stopped run still reports.
func (r boundRunner) runEndHook(ctx context.Context) func(string, error) {
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
		r.fireRunHook(detached, event)
	}
}
