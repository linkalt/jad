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
	"jad/backend-go/internal/outputcheck"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tracing"
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
	// The span ends after every other deferred step, the panic recovery included, so it
	// carries the run's final status.
	var runErr error
	ctx, span := tracing.StartRun(ctx, reference, profile, run.turnID, run.source)
	defer func() { tracing.EndRun(span, run.state(), runErr) }()
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
	defer h.watchAsyncState(run, reference, profile)()
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
	// The run's cost, from the pricing of the models that answered; its own meter (made by
	// Run) adds into this one, which the usage events report.
	meter := session.NewCostMeter(nil)
	ctx = session.WithCostMeter(ctx, meter)
	result, err := runner.Run(ctx, input, session.RunOptions{
		TurnID:          run.turnID,
		MemoryTrace:     h.memoryTrace,
		ToolExecution:   toolExecution,
		OutputFormat:    run.outputFormat,
		Plan:            run.plan,
		Tools:           run.tools,
		ExecutePlan:     run.executePlan,
		OnInputAccepted: func(input session.PersistedInput) { accepted = &input },
		Steer:           run.steer,
		OnSteer: func(items [][]byte) {
			if err := run.publish("steer", map[string]any{"turn_id": run.turnID, "items": rawItems(items)}); err != nil {
				logger.Warn("failed to publish steering message", slog.Any("error", err))
			}
		},
		OnSteerDropped: func(inputs []json.RawMessage) { publishSteerDropped(run, inputs, logger) },
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
			publishRunUsage(run, runner, ctx, meter, logger)
		},
		OnUsageChanged: func() { publishRunUsage(run, runner, ctx, meter, logger) },
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
			publishToolResult(run, normalizedRun, emitter, item, logger)
		},
	})
	// A run that failed before the session runner took its queue (a blocked prompt, a model
	// that did not load) never closed it: close it now, so a later message is refused, and
	// report the ones accepted that the run will never store.
	if dropped := run.steer.Close(); len(dropped) > 0 {
		publishSteerDropped(run, dropped, logger)
	}
	runErr = err
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
	var verdict *outputcheck.Verdict
	if run.outputValidator != nil {
		checked := outputcheck.Check(run.outputValidator, message.Text)
		verdict = &checked
	}
	terminal, err := encodeTerminalEvents(message, verdict)
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

// publishRunUsage sends the session's usage summary, and cost_usd, what this run's priced
// responses cost so far, once there is one.
func publishRunUsage(run *activeRun, runner RunSession, ctx context.Context, meter *session.CostMeter, logger *slog.Logger) {
	reader, ok := runner.(runUsageSummaryReader)
	if !ok {
		return
	}
	usage, err := reader.ReadRunUsageSummary(ctx)
	if err != nil {
		logger.Warn("failed to read usage summary", slog.Any("error", err))
		return
	}
	payload := map[string]any{"usage": usage}
	if cost, priced := meter.Total(); priced > 0 {
		payload["cost_usd"] = cost
	}
	if err := run.publish("usage", payload); err != nil {
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
// clientMessageError is an error whose ClientMessage holds nothing a client may not read.
type clientMessageError interface {
	ClientMessage() string
}

func clientRunErrorMessage(err error) string {
	var safe clientMessageError
	if errors.As(err, &safe) {
		return safe.ClientMessage()
	}
	// Its text holds only numbers the harness counted, so a client may read it.
	var tokenLimit *session.RunTokenLimitError
	if errors.As(err, &tokenLimit) {
		return tokenLimit.Error()
	}
	var costLimit *session.RunCostLimitError
	if errors.As(err, &costLimit) {
		return costLimit.Error()
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

// encodeTerminalEvents ends a completed run. A run sent with output_format gets the verdict
// on its answer as the completed event's output member.
func encodeTerminalEvents(message webUIMessage, verdict *outputcheck.Verdict) ([]streamEvent, error) {
	uiEvent, err := makeSSEEvent("ui.message", map[string]any{"message": message})
	if err != nil {
		return nil, err
	}
	completed := map[string]any{"message": message, "committed": true}
	if verdict != nil {
		completed["output"] = verdict
	}
	completedEvent, err := makeSSEEvent("completed", completed)
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

// publishToolResult shows a tool result the runner just settled: its inline images are kept
// for the live image route first, then the message part that counts them is published. One
// projection serves both, so the images are kept under the call id the part reports.
func publishToolResult(run *activeRun, streaming *webui.StreamingRun, emitter *messageStreamEmitter, item json.RawMessage, logger *slog.Logger) {
	part, ok := streaming.ProjectToolResult(item)
	if !ok {
		return
	}
	run.keepLiveImages(item, part)
	if err := emitter.push(streaming.AddToolResultPart(part)); err != nil {
		logging.Or(logger).Warn("failed to publish tool result", slog.Any("error", err))
	}
}

// publishSteerDropped reports steering messages that were accepted (202) but never stored
// because the run ended first, as a "steer_dropped" event ({turn_id, inputs}) and a warning,
// so the sender can send them again. Inline image and file data is left out of the event
// (withoutInlineData).
func publishSteerDropped(run *activeRun, inputs []json.RawMessage, logger *slog.Logger) {
	logger.Warn("steering messages dropped: the run ended before storing them", slog.Int("count", len(inputs)))
	if err := run.publish("steer_dropped", map[string]any{"turn_id": run.turnID, "inputs": withoutInlineData(inputs)}); err != nil {
		logger.Warn("failed to publish dropped steering messages", slog.Any("error", err))
	}
}
