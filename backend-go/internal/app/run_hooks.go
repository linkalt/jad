package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// fireRunHook runs the run_start or run_end commands of the session and writes each
// report to the server log. The reports never reach the model or messages.jsonl.
func (r boundRunner) fireRunHook(ctx context.Context, event native.RunEvent, onReport func(session.HookReport)) {
	fireSessionHook(ctx, r.resolver.logger, r.reference, r.config, event, onReport)
}

// fireSessionHook fills in the session fields of event, runs its commands, logs each report
// and hands it to onReport when that is set.
func fireSessionHook(ctx context.Context, logger *slog.Logger, reference string, config sessionfs.SessionConfig, event native.RunEvent, onReport func(session.HookReport)) native.RunEventResult {
	return fireSessionHookWithKey(ctx, logger, reference, config, config.Model.APIKeyEnv, event, onReport)
}

// fireSessionHookWithKey is fireSessionHook for a caller that knows the model's key variable
// when config does not hold the model yet; the variable is kept out of the commands' environment.
func fireSessionHookWithKey(ctx context.Context, logger *slog.Logger, reference string, config sessionfs.SessionConfig, apiKeyEnv string, event native.RunEvent, onReport func(session.HookReport)) native.RunEventResult {
	event.Session = reference
	event.Profile = config.Profile
	event.WorkspaceRoot = config.WorkingDir
	result := native.FireRunEventResult(ctx, config.Hooks, apiKeyEnv, event)
	for _, report := range result.Reports {
		logger.Info("hook "+event.Event,
			slog.String("session", reference), slog.String("profile", config.Profile),
			slog.String("turn_id", event.TurnID), slog.String("trigger", event.Trigger),
			slog.String("report", report))
		if onReport != nil {
			onReport(session.HookReport{Event: event.Event, Trigger: event.Trigger, Report: report})
		}
	}
	return result
}

// blockedByHook returns the error of an event a blocking command stopped, nil when none did.
func blockedByHook(event string, result native.RunEventResult) error {
	if result.BlockedBy == "" {
		return nil
	}
	return &native.HookBlockedError{Event: event, Hook: result.BlockedBy, Report: strings.Join(result.Reports, "\n")}
}

// promptSubmitInput runs the prompt_submit commands for one run's input, before it is
// stored. It returns the input to store: input itself, or its items followed by one
// message (role hooks.prompt_context_role) that holds what the context commands printed.
// A blocking command that does not exit 0 returns a *native.HookBlockedError, and the
// caller stores nothing. An input that does not parse is returned as it is, for the store
// to refuse with its own error.
func (r boundRunner) promptSubmitInput(ctx context.Context, input json.RawMessage, turnID string, onReport func(session.HookReport)) (json.RawMessage, error) {
	hooks := r.config.Hooks
	if !hooks.Enabled || len(hooks.PromptSubmit) == 0 {
		return input, nil
	}
	items, err := session.NormalizeInput(input)
	if err != nil {
		return input, nil
	}
	// The model is loaded later in the run; its key variable is read here only to keep it
	// out of the commands' environment, as for every other hook.
	apiKeyEnv := r.config.Model.APIKeyEnv
	if withModel, err := r.config.WithModel(); err == nil {
		apiKeyEnv = withModel.Model.APIKeyEnv
	}
	event := native.RunEvent{Event: native.HookEventPromptSubmit, TurnID: turnID, Prompt: inputPromptText(items)}
	result := fireSessionHookWithKey(ctx, r.resolver.logger, r.reference, r.config, apiKeyEnv, event, onReport)
	if err := blockedByHook(native.HookEventPromptSubmit, result); err != nil {
		return nil, err
	}
	if len(result.Context) == 0 {
		return input, nil
	}
	type textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	contextItem, err := json.Marshal(struct {
		Type    string     `json:"type"`
		Role    string     `json:"role"`
		Content []textPart `json:"content"`
	}{"message", hooks.PromptContextRole, []textPart{{"input_text", native.PromptContextText(result.Context, hooks.MaxOutputBytes)}}})
	if err != nil {
		return nil, fmt.Errorf("encode prompt_submit context: %w", err)
	}
	list := make([]json.RawMessage, 0, len(items)+1)
	for _, item := range items {
		list = append(list, item)
	}
	return json.Marshal(append(list, contextItem))
}

// inputPromptText is the text a prompt_submit command reads as the prompt: the input_text
// parts of the input's user messages, one per line.
func inputPromptText(items [][]byte) string {
	var parts []string
	for _, item := range items {
		var message struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(item, &message) != nil || message.Type != "message" || message.Role != "user" {
			continue
		}
		for _, part := range message.Content {
			if part.Type == "input_text" && part.Text != "" {
				parts = append(parts, part.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
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
