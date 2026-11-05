// Package tracing records OpenTelemetry spans for runs, model requests and tool calls and
// exports them over OTLP/HTTP, JSON encoding, when server.tracing is enabled.
//
// The span shape follows the OpenTelemetry GenAI conventions where they apply
// (gen_ai.operation.name "chat" and "execute_tool", gen_ai.request.model,
// gen_ai.usage.*, gen_ai.tool.*), plus jad.* attributes for what JAD adds (session,
// profile, turn, round). Spans never carry prompts, answers, tool arguments or tool output:
// only names, ids, sizes, token counts and outcomes.
//
// Until Setup installs a provider, the global OpenTelemetry provider is the no-op one, so
// the Start functions return spans that record nothing and the End functions skip every
// decode: tracing costs a few calls per span when it is off.
package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"jad/backend-go/internal/sessionfs"
)

// instrumentationName names the tracer, as the instrumentation scope of every span.
const instrumentationName = "jad/backend-go"

// tracesPath is the OTLP/HTTP path appended to a configured endpoint.
const tracesPath = "/v1/traces"

// Setup installs the global tracer provider for settings and returns the function that
// flushes and stops it; call that before the process exits, or buffered spans are lost.
// Shutdown waits at most for the context it is given; see Shutdown.
// With tracing disabled it installs nothing and the shutdown does nothing. Export errors
// are logged at warn level and never fail a run.
func Setup(settings sessionfs.TracingSettings, logger *slog.Logger) (func(context.Context) error, error) {
	if !settings.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	provider, err := NewProvider(settings, nil)
	if err != nil {
		return nil, err
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if logger != nil {
			logger.Warn("tracing export failed", slog.Any("error", err))
		}
	}))
	restore := Install(provider)
	return func(ctx context.Context) error {
		err := provider.Shutdown(ctx)
		restore()
		return err
	}, nil
}

// installed is true once Install has run; until then the Start functions return a span
// that records nothing without building attributes.
var installed atomic.Bool

// Install makes provider the global tracer provider and turns span recording on. restore
// puts the previous provider and state back.
func Install(provider trace.TracerProvider) (restore func()) {
	previous, wasInstalled := otel.GetTracerProvider(), installed.Load()
	otel.SetTracerProvider(provider)
	installed.Store(true)
	return func() {
		// The default global provider cannot be set again (otel refuses a delegate to
		// itself), so a process that had none gets an explicit no-op one.
		if !wasInstalled {
			previous = noop.NewTracerProvider()
		}
		otel.SetTracerProvider(previous)
		installed.Store(wasInstalled)
	}
}

// inactive is the span the Start functions return while tracing is off.
var inactive = trace.SpanFromContext(context.Background())

// NewProvider builds the tracer provider for settings. exporter, when set, replaces the
// OTLP/HTTP JSON exporter (a test passes an in-memory one).
func NewProvider(settings sessionfs.TracingSettings, exporter sdktrace.SpanExporter) (*sdktrace.TracerProvider, error) {
	if exporter == nil {
		created, err := newJSONExporter(settings.Endpoint, os.Getenv)
		if err != nil {
			return nil, fmt.Errorf("server.tracing: %w", err)
		}
		exporter = created
	}
	described, err := resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", settings.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("server.tracing: describe the service: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(described),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(settings.SampleRatio))),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(time.Duration(settings.BatchTimeoutMS)*time.Millisecond),
			sdktrace.WithExportTimeout(time.Duration(settings.ExportTimeoutMS)*time.Millisecond),
			sdktrace.WithMaxQueueSize(int(settings.MaxQueueSize)),
		),
	), nil
}

func tracer() trace.Tracer { return otel.Tracer(instrumentationName) }

// StartRun starts the span of one run of a session profile. source names who started it
// (empty for the web client).
func StartRun(ctx context.Context, session, profile, turnID, source string) (context.Context, trace.Span) {
	ctx, span := startRunSpan(ctx, session, profile, turnID, source)
	span = withMetrics(span, func(recorded *metricSpan) {
		recorded.source = source
		if recorded.source == "" {
			recorded.source = "web"
		}
	})
	recordRunStart(span)
	return ctx, span
}

func startRunSpan(ctx context.Context, session, profile, turnID, source string) (context.Context, trace.Span) {
	if !installed.Load() {
		return ctx, inactive
	}
	attributes := []attribute.KeyValue{attribute.String("jad.session", session), attribute.String("jad.turn_id", turnID)}
	if profile != "" {
		attributes = append(attributes, attribute.String("jad.profile", profile))
	}
	if source != "" {
		attributes = append(attributes, attribute.String("jad.run.source", source))
	}
	return tracer().Start(ctx, "jad.run", trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attributes...))
}

// EndRun ends a run span with the run's final status: completed, failed or aborted.
func EndRun(span trace.Span, status string, err error) {
	recordRunEnd(span, status)
	if span.IsRecording() {
		span.SetAttributes(attribute.String("jad.run.status", status))
		switch {
		case status == "failed":
			setError(span, err, "run failed")
		case status == "aborted":
			span.SetStatus(codes.Unset, "")
		default:
			span.SetStatus(codes.Ok, "")
		}
	}
	span.End()
}

// StartModelRequest starts the span of one model request (one round of a run).
func StartModelRequest(ctx context.Context, model string, round int) (context.Context, trace.Span) {
	ctx, span := startModelRequestSpan(ctx, model, round)
	return ctx, withMetrics(span, func(recorded *metricSpan) { recorded.model = model })
}

func startModelRequestSpan(ctx context.Context, model string, round int) (context.Context, trace.Span) {
	if !installed.Load() {
		return ctx, inactive
	}
	name := "chat"
	if model != "" {
		name += " " + model
	}
	return tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", model),
		attribute.Int("jad.round", round),
	))
}

// EndModelRequest ends a model request span. event is the terminal Responses event of the
// request (response.completed); its id, model and usage are recorded.
func EndModelRequest(span trace.Span, event json.RawMessage, err error) {
	recordModelRequest(span, event, err)
	if span.IsRecording() {
		if err != nil {
			setError(span, err, "model request failed")
		} else {
			span.SetAttributes(responseAttributes(event)...)
		}
	}
	span.End()
}

// responseAttributes reads the response id, model, status and token usage of a terminal
// Responses event.
func responseAttributes(event json.RawMessage) []attribute.KeyValue {
	var payload struct {
		Response struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
			Usage  *struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(event, &payload) != nil {
		return nil
	}
	response := payload.Response
	var attributes []attribute.KeyValue
	if response.ID != "" {
		attributes = append(attributes, attribute.String("gen_ai.response.id", response.ID))
	}
	if response.Model != "" {
		attributes = append(attributes, attribute.String("gen_ai.response.model", response.Model))
	}
	if response.Status != "" {
		attributes = append(attributes, attribute.StringSlice("gen_ai.response.finish_reasons", []string{response.Status}))
	}
	if usage := response.Usage; usage != nil {
		attributes = append(attributes,
			attribute.Int64("gen_ai.usage.input_tokens", usage.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", usage.OutputTokens),
			attribute.Int64("gen_ai.usage.cache_read.input_tokens", usage.InputTokensDetails.CachedTokens),
			attribute.Int64("gen_ai.usage.reasoning.output_tokens", usage.OutputTokensDetails.ReasoningTokens),
		)
	}
	return attributes
}

// StartToolCall starts the span of one tool call. call is the function_call item; only
// its name and call id are read.
func StartToolCall(ctx context.Context, call json.RawMessage) (context.Context, trace.Span) {
	if !installed.Load() && meter.Load() == nil {
		return ctx, inactive
	}
	var item struct {
		Name   string `json:"name"`
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(call, &item)
	ctx, span := startToolCallSpan(ctx, item.Name, item.CallID)
	return ctx, withMetrics(span, func(recorded *metricSpan) { recorded.tool = item.Name })
}

func startToolCallSpan(ctx context.Context, tool, callID string) (context.Context, trace.Span) {
	if !installed.Load() {
		return ctx, inactive
	}
	name := "execute_tool"
	if tool != "" {
		name += " " + tool
	}
	return tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", tool),
		attribute.String("gen_ai.tool.call.id", callID),
	))
}

// EndToolCall ends a tool call span with the size of the output item.
func EndToolCall(span trace.Span, output json.RawMessage, err error) {
	recordToolCall(span, output, err)
	if span.IsRecording() {
		span.SetAttributes(attribute.Int("jad.tool.output_bytes", len(output)))
		if err != nil {
			setError(span, err, "tool call failed")
		}
	}
	span.End()
}

// setError marks a span failed. The error text is not recorded when it may hold provider
// or tool detail; only its kind is, as the client-safe description of a failed run is.
func setError(span trace.Span, err error, description string) {
	span.SetAttributes(attribute.String("error.type", errorKind(err)))
	span.SetStatus(codes.Error, description)
}

// errorKind is the error.type of err: canceled, deadline_exceeded or the Go type.
func errorKind(err error) string {
	switch {
	case err == nil:
		return "error"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return fmt.Sprintf("%T", err)
	}
}

// Shutdown calls shutdown with a deadline of settings.export_timeout_ms, so a collector that
// does not answer cannot hold the process at exit.
func Shutdown(shutdown func(context.Context) error, settings sessionfs.TracingSettings) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.ExportTimeoutMS)*time.Millisecond)
	defer cancel()
	return shutdown(ctx)
}
