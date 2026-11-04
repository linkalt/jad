package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"jad/backend-go/internal/sessionfs"
)

// recordSpans installs a provider that keeps the ended spans in memory, synchronously.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	restore := Install(provider)
	t.Cleanup(func() {
		restore()
		_ = provider.Shutdown(context.Background())
	})
	return exporter
}

func attributesOf(span tracetest.SpanStub) map[attribute.Key]attribute.Value {
	values := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes {
		values[kv.Key] = kv.Value
	}
	return values
}

func TestSpansNestAndCarryTheGenAIAttributes(t *testing.T) {
	exporter := recordSpans(t)
	ctx, run := StartRun(context.Background(), "local:demo", "dev/coder", "web-1", "")
	roundCtx, model := StartModelRequest(ctx, "gpt-x", 0)
	_ = roundCtx
	EndModelRequest(model, json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-x-2026","status":"completed","usage":{"input_tokens":120,"output_tokens":30,"input_tokens_details":{"cached_tokens":100},"output_tokens_details":{"reasoning_tokens":7}}}}`), nil)
	_, tool := StartToolCall(ctx, json.RawMessage(`{"type":"function_call","name":"read","call_id":"call_9","arguments":"{\"path\":\"secret.txt\"}"}`))
	EndToolCall(tool, json.RawMessage(`{"type":"function_call_output","call_id":"call_9","output":"file body"}`), nil)
	EndRun(run, "completed", nil)

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, span := range spans {
		byName[span.Name] = span
	}
	root, chat, read := byName["jad.run"], byName["chat gpt-x"], byName["execute_tool read"]
	if root.Name == "" || chat.Name == "" || read.Name == "" {
		t.Fatalf("span names = %v", byName)
	}
	for _, child := range []tracetest.SpanStub{chat, read} {
		if child.Parent.SpanID() != root.SpanContext.SpanID() || child.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Fatalf("%s is not a child of jad.run", child.Name)
		}
	}
	runAttrs := attributesOf(root)
	if runAttrs["jad.session"].AsString() != "local:demo" || runAttrs["jad.profile"].AsString() != "dev/coder" || runAttrs["jad.run.status"].AsString() != "completed" || root.Status.Code != codes.Ok {
		t.Fatalf("run span = %v %v", runAttrs, root.Status)
	}
	chatAttrs := attributesOf(chat)
	for key, want := range map[attribute.Key]any{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "gpt-x", "gen_ai.response.model": "gpt-x-2026", "gen_ai.response.id": "resp_1",
		"gen_ai.usage.input_tokens": int64(120), "gen_ai.usage.output_tokens": int64(30), "gen_ai.usage.cache_read.input_tokens": int64(100),
		"gen_ai.usage.reasoning.output_tokens": int64(7), "jad.round": int64(0),
	} {
		if got := chatAttrs[key].AsInterface(); got != want {
			t.Errorf("chat %s = %v, want %v", key, got, want)
		}
	}
	toolAttrs := attributesOf(read)
	if toolAttrs["gen_ai.tool.name"].AsString() != "read" || toolAttrs["gen_ai.tool.call.id"].AsString() != "call_9" || toolAttrs["jad.tool.output_bytes"].AsInt64() == 0 {
		t.Fatalf("tool span = %v", toolAttrs)
	}
	// Content never reaches a span: not the arguments, not the output.
	for _, span := range spans {
		for _, kv := range span.Attributes {
			if text := kv.Value.Emit(); text == "file body" || text == `{"path":"secret.txt"}` {
				t.Fatalf("%s carries content in %s", span.Name, kv.Key)
			}
		}
	}
}

func TestFailedSpansRecordTheErrorKindOnly(t *testing.T) {
	exporter := recordSpans(t)
	ctx, run := StartRun(context.Background(), "s", "", "t", "profile_message")
	_, model := StartModelRequest(ctx, "m", 2)
	EndModelRequest(model, nil, errors.New("provider said: secret detail"))
	_, tool := StartToolCall(ctx, json.RawMessage(`{"name":"bash","call_id":"c"}`))
	EndToolCall(tool, nil, context.Canceled)
	EndRun(run, "failed", errors.New("boom"))
	for _, span := range exporter.GetSpans() {
		if span.Status.Code != codes.Error {
			t.Errorf("%s status = %v, want error", span.Name, span.Status)
		}
		for _, kv := range span.Attributes {
			if kv.Value.Emit() == "provider said: secret detail" || kv.Value.Emit() == "boom" {
				t.Errorf("%s carries the error text", span.Name)
			}
		}
		if span.Name == "execute_tool bash" && attributesOf(span)["error.type"].AsString() != "canceled" {
			t.Errorf("tool error.type = %v", attributesOf(span)["error.type"])
		}
		if span.Name == "jad.run" && attributesOf(span)["jad.run.source"].AsString() != "profile_message" {
			t.Errorf("run source = %v", attributesOf(span)["jad.run.source"])
		}
	}
	_, aborted := StartRun(context.Background(), "s", "", "t2", "")
	EndRun(aborted, "aborted", nil)
	last := exporter.GetSpans()[len(exporter.GetSpans())-1]
	if last.Status.Code != codes.Unset {
		t.Fatalf("aborted run status = %v, want unset", last.Status)
	}
}

// Off, the Start and End functions allocate nothing and the context is passed through.
func TestTracingOffAllocatesNothing(t *testing.T) {
	if installed.Load() {
		t.Fatal("a provider is installed")
	}
	call := json.RawMessage(`{"name":"read","call_id":"c"}`)
	event := json.RawMessage(`{"response":{"usage":{"input_tokens":1}}}`)
	ctx := context.Background()
	allocations := testing.AllocsPerRun(100, func() {
		runCtx, run := StartRun(ctx, "s", "p", "t", "")
		_, model := StartModelRequest(runCtx, "m", 0)
		EndModelRequest(model, event, nil)
		_, tool := StartToolCall(runCtx, call)
		EndToolCall(tool, call, nil)
		EndRun(run, "completed", nil)
		if runCtx != ctx {
			t.Fatal("the context changed")
		}
	})
	if allocations != 0 {
		t.Fatalf("allocations per run = %v, want 0", allocations)
	}
}

func TestSetupDisabledInstallsNothingAndShutdownRestores(t *testing.T) {
	shutdown, err := Setup(sessionfs.TracingSettings{}, nil)
	if err != nil || installed.Load() {
		t.Fatalf("disabled setup: installed = %v, err = %v", installed.Load(), err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	settings := sessionfs.TracingSettings{Enabled: true, Endpoint: "http://127.0.0.1:1", ServiceName: "jad-test", SampleRatio: 1, ExportTimeoutMS: 100, BatchTimeoutMS: 100, MaxQueueSize: 16}
	shutdown, err = Setup(settings, nil)
	if err != nil || !installed.Load() {
		t.Fatalf("enabled setup: installed = %v, err = %v", installed.Load(), err)
	}
	_, span := StartRun(context.Background(), "s", "", "t", "")
	EndRun(span, "completed", nil)
	// Nothing listens on the endpoint: the flush fails within the export timeout, and the
	// provider is uninstalled either way.
	_ = Shutdown(shutdown, settings)
	if installed.Load() {
		t.Fatal("shutdown left the provider installed")
	}
}
