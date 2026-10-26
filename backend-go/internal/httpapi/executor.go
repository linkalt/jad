package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

type webUIMessage = webui.Message

// runUsageSummaryReader reads usage from inside a run. Like the TypeScript run,
// the usage is republished after every committed response and after an
// automatic compaction, so a client sees the totals grow across tool rounds.
type runUsageSummaryReader interface {
	ReadRunUsageSummary(context.Context) (webui.UsageSummary, error)
}

func (h *Handler) executeRun(run *activeRun, runner RunSession, input json.RawMessage) {
	// The retry notifier rides in the context: the client that retries is shared by every
	// run, and only this run should show the wait.
	ctx := responses.WithRetryNotifier(run.context(), run.setProviderRetry)
	logger := logging.Or(h.logger).With(runLogAttrs(run)...)
	reference, profile, _ := strings.Cut(run.key, "\x00")
	defer h.watchAsyncState(run, reference, profile)()
	started := time.Now()
	logger.Info("run started")
	// The turn and message the run's user input was saved as, once it is saved.
	// A failure is kept beside the transcript under them, so it outlives the
	// run-state window. Run calls OnInputAccepted on this goroutine.
	var accepted *session.PersistedInput
	recordFailure := func(message string) {
		recordRunFailure(runner, accepted, run.turnID, message, logger)
	}
	// A panic in the provider, a tool or an observer must fail this run, not the
	// process. finish is idempotent, so a run that already ended keeps its status.
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("run panicked", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
			message := clientRunErrorMessage(fmt.Errorf("internal error"))
			recordFailure(message)
			run.finish("failed", failureEvents(message))
		}
		logger.Info("run finished", slog.String("status", run.state()), slog.Float64("duration_ms", durationMS(time.Since(started))))
	}()
	timestamp := time.Now().Local().Format("3:04 PM")
	// No empty placeholder message is published: like the TypeScript run, the first
	// ui.message is the first visible change, so a client that attaches before any
	// output receives only the run summary.
	toolExecution := tools.ExecutionContext{}
	run.mu.Lock()
	toolExecution.RemoteHops = run.hops
	run.mu.Unlock()
	if run.canRequestInteraction() && run.interactions != nil {
		toolExecution.RequestInteraction = run.interactions.Request
	}
	normalizedRun := webui.NewStreamingRun(run.turnID, timestamp)
	normalizedRun.SetHostedRawLimit(h.maxInlinePartBytes)
	emitter := newMessageStreamEmitter(h.toolStreamInterval, run.publishMessage)
	emitter.logger = logger
	defer emitter.discard()
	result, err := runner.Run(ctx, input, session.RunOptions{
		TurnID:          run.turnID,
		MemoryTrace:     h.memoryTrace,
		ToolExecution:   toolExecution,
		OnInputAccepted: func(input session.PersistedInput) { accepted = &input },
		OnTurnStarted: func(partialContentPath string) {
			run.setPartialContentPath(partialContentPath)
		},
		OnResponseCommitted: func(event json.RawMessage, _ transcript.CommitResult) {
			if run.onCommitted != nil {
				run.onCommitted(event)
			}
			if err := emitter.flush(); err != nil {
				logger.Warn("failed to flush committed message", slog.Any("error", err))
			}
			run.setCommittedMessage(normalizedRun.Message("streaming"))
			publishRunUsage(run, runner, ctx, logger)
		},
		OnUsageChanged: func() { publishRunUsage(run, runner, ctx, logger) },
		OnHookReport: func(report session.HookReport) {
			if err := run.publish("hook_report", map[string]any{"turn_id": run.turnID, "event": report.Event, "trigger": report.Trigger, "report": report.Report}); err != nil {
				logger.Warn("failed to publish hook report", slog.Any("error", err))
			}
		},
		OnEvent: func(event responses.Event) error {
			if event.Type == "response.output_text.delta" {
				var payload struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(event.Raw, &payload) == nil {
					run.cacheContent(payload.Delta)
				}
			}
			message, changed := normalizedRun.Update(event.Raw)
			if !changed {
				return nil
			}
			return emitter.push(message)
		},
		OnToolResult: func(item json.RawMessage) {
			run.keepLiveImages(item)
			message, changed := normalizedRun.AddToolResult(item)
			if changed {
				if err := emitter.push(message); err != nil {
					logger.Warn("failed to publish tool result", slog.Any("error", err))
				}
			}
		},
	})
	if err != nil {
		// Like the TypeScript run, a failed or stopped run drops the snapshot still
		// waiting in the coalescing window instead of publishing it: it describes
		// text the run never committed, and the terminal error follows.
		emitter.discard()
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			logger.Info("run aborted", slog.String("reason", abortReason(ctx)))
			run.finish("aborted", failureEvents("The run was stopped before it completed."))
			return
		}
		logger.Error("run failed", slog.Any("error", err))
		failure := clientRunErrorMessage(err)
		recordFailure(failure)
		run.finish("failed", failureEvents(failure))
		return
	}
	if run.onResult != nil {
		run.onResult(result.Event)
	}
	message := normalizedRun.Complete(result.Event)
	terminal, err := encodeTerminalEvents(message)
	if err != nil {
		emitter.discard()
		logger.Error("run failed", slog.Any("error", err))
		failure := clientRunErrorMessage(err)
		recordFailure(failure)
		run.finish("failed", failureEvents(failure))
		return
	}
	// The terminal full message supersedes any append-only snapshot still waiting.
	emitter.discard()
	run.finish("completed", terminal)
}

func publishRunUsage(run *activeRun, runner RunSession, ctx context.Context, logger *slog.Logger) {
	reader, ok := runner.(runUsageSummaryReader)
	if !ok {
		return
	}
	usage, err := reader.ReadRunUsageSummary(ctx)
	if err != nil {
		logger.Warn("failed to read usage summary", slog.Any("error", err))
		return
	}
	if err := run.publish("usage", map[string]any{"usage": usage}); err != nil {
		logger.Warn("failed to publish usage summary", slog.Any("error", err))
	}
}

// runErrorRecorder is implemented by a run session that keeps failed runs in a
// cache beside the transcript. A session without one simply records nothing.
type runErrorRecorder interface {
	RecordRunError(transcript.RunErrorEntry) error
}

// recordRunFailure keeps one failed run, never changing the run's outcome: a
// write problem is only logged. Nothing is recorded when the user input was not
// saved, since there is no turn for the failure to belong to.
func recordRunFailure(runner RunSession, accepted *session.PersistedInput, turnID, message string, logger *slog.Logger) {
	recorder, ok := runner.(runErrorRecorder)
	if !ok || accepted == nil {
		return
	}
	entry := transcript.RunErrorEntry{
		Turn:    accepted.SessionTurn,
		TurnID:  turnID,
		At:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Message: message,
	}
	if accepted.HasMessage {
		entry.MessageID = fmt.Sprintf("session-user-o%d", accepted.MessageOffset)
	}
	if err := recorder.RecordRunError(entry); err != nil {
		logger.Warn("failed to record run error", slog.Any("error", err))
	}
}

// providerStatusError is implemented by an error that carries the HTTP status a
// provider answered with.
type providerStatusError interface{ ProviderStatus() int }

// streamIdleError is implemented by an error for a provider stream that went silent.
type streamIdleError interface{ IdleTimeout() time.Duration }

// clientRunErrorMessage is the only description of a failed run a client gets:
// the provider's HTTP status when there was one, otherwise a generic line. The
// detail (a response body, a path, a tool server's message) stays in the server
// log. The wording matches the TypeScript backend.
func clientRunErrorMessage(err error) string {
	// Its text holds only numbers the harness counted, so a client may read it.
	var tokenLimit *session.RunTokenLimitError
	if errors.As(err, &tokenLimit) {
		return tokenLimit.Error()
	}
	var idle streamIdleError
	if errors.As(err, &idle) {
		return fmt.Sprintf("The model provider sent nothing for %s, so the run was ended. Details are in the server log.", idle.IdleTimeout().Round(time.Second))
	}
	var provider providerStatusError
	if errors.As(err, &provider) {
		if status := provider.ProviderStatus(); status >= 100 && status <= 599 {
			return fmt.Sprintf("The provider request failed (HTTP %d). Response details were withheld.", status)
		}
	}
	return "The run failed. Response details were withheld; check server logs for diagnostics."
}

func failureEvents(message string) []streamEvent {
	errorEvent, _ := makeSSEEvent("error", map[string]string{"message": message})
	doneEvent, _ := makeSSEEvent("done", struct{}{})
	return []streamEvent{errorEvent, doneEvent}
}

func encodeTerminalEvents(message webUIMessage) ([]streamEvent, error) {
	uiEvent, err := makeSSEEvent("ui.message", map[string]any{"message": message})
	if err != nil {
		return nil, err
	}
	completedEvent, err := makeSSEEvent("completed", map[string]any{"message": message, "committed": true})
	if err != nil {
		return nil, err
	}
	doneEvent, err := makeSSEEvent("done", struct{}{})
	if err != nil {
		return nil, err
	}
	return []streamEvent{uiEvent, completedEvent, doneEvent}, nil
}

// runLogAttrs identifies a run in the log by its session reference, profile and
// turn. The run key is "reference\x00profile"; an empty profile logs as "-".
func runLogAttrs(run *activeRun) []any {
	reference, profile, _ := strings.Cut(run.key, "\x00")
	if profile == "" {
		profile = "-"
	}
	return []any{slog.String("session", reference), slog.String("profile", profile), slog.String("turn", run.turnID)}
}

// abortReason names why a run's context ended, like the TypeScript
// "[agent-run] aborted ... reason=" line.
func abortReason(ctx context.Context) string {
	if cause := context.Cause(ctx); cause != nil {
		return cause.Error()
	}
	return "unspecified"
}
