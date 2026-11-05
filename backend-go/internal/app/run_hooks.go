package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
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
	if result.Prompt != nil {
		if items, err = rewritePromptText(items, *result.Prompt); err != nil {
			return nil, fmt.Errorf("apply the prompt_submit hook %s's new prompt: %w", result.RewrittenBy, err)
		}
		if len(result.Context) == 0 {
			return encodeInputItems(items)
		}
	}
	if len(result.Context) == 0 {
		return input, nil
	}
	return appendHookContext(items, hooks.PromptContextRole, native.HookContextText(native.HookEventPromptSubmit, result.Context, hooks.MaxOutputBytes))
}

// sessionStartInput runs the session_start commands before the first run of a
// conversation, when the transcript holds no turn yet. What their context commands print
// is stored after the input in one message of role hooks.prompt_context_role.
func (r boundRunner) sessionStartInput(ctx context.Context, input json.RawMessage, turnID string, nextSessionTurn func() (int64, error), onReport func(session.HookReport)) (json.RawMessage, error) {
	hooks := r.config.Hooks
	if !hooks.Enabled || len(hooks.SessionStart) == 0 {
		return input, nil
	}
	turn, err := nextSessionTurn()
	if err != nil || turn != 1 {
		return input, nil
	}
	items, err := session.NormalizeInput(input)
	if err != nil {
		return input, nil
	}
	result := fireSessionHookWithKey(ctx, r.resolver.logger, r.reference, r.config, r.hookAPIKeyEnv(), native.RunEvent{Event: native.HookEventSessionStart, TurnID: turnID, Prompt: inputPromptText(items)}, onReport)
	if len(result.Context) == 0 {
		return input, nil
	}
	return appendHookContext(items, hooks.PromptContextRole, native.HookContextText(native.HookEventSessionStart, result.Context, hooks.MaxOutputBytes))
}

// hookAPIKeyEnv is the model's key variable, kept out of the commands' environment; the
// model is loaded later in the run, so it is read here only for that.
func (r boundRunner) hookAPIKeyEnv() string {
	if withModel, err := r.config.WithModel(); err == nil {
		return withModel.Model.APIKeyEnv
	}
	return r.config.Model.APIKeyEnv
}

// applyStopHooks makes the run ask the stop commands before it ends.
func (r boundRunner) applyStopHooks(options *session.RunOptions) {
	hooks := r.config.Hooks
	if !hooks.Enabled || len(hooks.Stop) == 0 || hooks.StopMaxContinues == 0 {
		return
	}
	options.StopMaxContinues = int(hooks.StopMaxContinues)
	options.StopMessageRole = hooks.StopMessageRole
	onReport := options.OnHookReport
	options.OnStop = func(ctx context.Context, turnID, answer string, continues int) (string, error) {
		result := fireSessionHook(ctx, r.resolver.logger, r.reference, r.config, native.RunEvent{Event: native.HookEventStop, TurnID: turnID, Answer: answer, Continues: continues}, onReport)
		if result.Continue == "" {
			return "", nil
		}
		return "[stop hook " + result.ContinuedBy + "]\n" + result.Continue, nil
	}
	// A stop command skipped because a cap was reached is reported like a command's
	// report, so a stop hook used as a check does not pass unseen.
	options.OnStopSkipped = func(_ context.Context, turnID string, limit error) {
		report := "[stop] not run: " + limit.Error()
		r.resolver.logger.Warn("hook "+native.HookEventStop,
			slog.String("session", r.reference), slog.String("profile", r.config.Profile),
			slog.String("turn_id", turnID), slog.String("trigger", stopSkippedTrigger),
			slog.String("report", report))
		if onReport != nil {
			onReport(session.HookReport{Event: native.HookEventStop, Trigger: stopSkippedTrigger, Report: report})
		}
	}
}

// stopSkippedTrigger is the trigger of the hook report that says the stop commands were not
// run because the run reached a cap.
const stopSkippedTrigger = "cap_reached"

// applyNotificationHooks fires the notification commands each time a tool of the run asks
// the user a question. They run beside the question, on a context the run's end does not
// cancel, so a slow command never holds the question back.
func (r boundRunner) applyNotificationHooks(ctx context.Context, options *session.RunOptions) {
	hooks := r.config.Hooks
	inner := options.ToolExecution.RequestInteraction
	if !hooks.Enabled || len(hooks.Notification) == 0 || inner == nil {
		return
	}
	detached := context.WithoutCancel(ctx)
	turnID, onReport := options.TurnID, options.OnHookReport
	options.ToolExecution.RequestInteraction = func(ctx context.Context, identity tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
		go fireSessionHook(detached, r.resolver.logger, r.reference, r.config, native.RunEvent{
			Event: native.HookEventNotification, Trigger: native.NotificationInteraction, TurnID: turnID,
			Tool: identity.Tool, CallID: identity.ToolCallID, Title: question.Title,
		}, onReport)
		return inner(ctx, identity, question)
	}
}

// appendHookContext returns items followed by one message of role that holds text.
func appendHookContext(items [][]byte, role, text string) (json.RawMessage, error) {
	contextItem, err := json.Marshal(struct {
		Type    string         `json:"type"`
		Role    string         `json:"role"`
		Content []hookTextPart `json:"content"`
	}{"message", role, []hookTextPart{{"input_text", text}}})
	if err != nil {
		return nil, fmt.Errorf("encode hook context: %w", err)
	}
	return encodeInputItems(append(items, contextItem))
}

type hookTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func encodeInputItems(items [][]byte) (json.RawMessage, error) {
	list := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		list = append(list, item)
	}
	return json.Marshal(list)
}

// rewritePromptText replaces the prompt text of the input: the input_text parts of its
// first user message become one part holding prompt, and the input_text parts of later
// user messages are dropped. Other parts (images, files) and other items are kept.
func rewritePromptText(items [][]byte, prompt string) ([][]byte, error) {
	rewritten := make([][]byte, 0, len(items))
	placed := false
	for _, item := range items {
		var message map[string]json.RawMessage
		var kind, role string
		if json.Unmarshal(item, &message) != nil || json.Unmarshal(message["type"], &kind) != nil || json.Unmarshal(message["role"], &role) != nil || kind != "message" || role != "user" {
			rewritten = append(rewritten, item)
			continue
		}
		var parts []json.RawMessage
		if json.Unmarshal(message["content"], &parts) != nil {
			rewritten = append(rewritten, item)
			continue
		}
		kept := make([]json.RawMessage, 0, len(parts)+1)
		if !placed {
			text, err := json.Marshal(hookTextPart{"input_text", prompt})
			if err != nil {
				return nil, err
			}
			kept = append(kept, text)
			placed = true
		}
		for _, part := range parts {
			var header struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(part, &header) == nil && header.Type == "input_text" {
				continue
			}
			kept = append(kept, part)
		}
		if len(kept) == 0 {
			continue
		}
		content, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		message["content"] = content
		rewritten = append(rewritten, encodeMessageOrdered(message))
	}
	if !placed {
		return nil, errors.New("the input has no user message to hold the new prompt")
	}
	return rewritten, nil
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

// encodeMessageOrdered writes a message item with type, role and content first, as input
// items are written, then its other fields in name order.
func encodeMessageOrdered(message map[string]json.RawMessage) []byte {
	var builder strings.Builder
	builder.WriteByte('{')
	first := true
	write := func(name string, value json.RawMessage) {
		if !first {
			builder.WriteByte(',')
		}
		first = false
		encodedName, _ := json.Marshal(name)
		builder.Write(encodedName)
		builder.WriteByte(':')
		builder.Write(value)
	}
	for _, name := range []string{"type", "role", "content"} {
		if value, exists := message[name]; exists {
			write(name, value)
		}
	}
	rest := make([]string, 0, len(message))
	for name := range message {
		if name != "type" && name != "role" && name != "content" {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		write(name, message[name])
	}
	builder.WriteByte('}')
	return []byte(builder.String())
}
