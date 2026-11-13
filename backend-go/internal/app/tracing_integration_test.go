package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/tracing"
)

// A run that calls a tool is one trace: the run span, a chat span per round and the tool
// call's span, all children of the run. Spans carry the model and the tool name, not the
// file content the tool returned.
func TestTracingRecordsARunWithItsRoundsAndToolCalls(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	restore := tracing.Install(provider)
	t.Cleanup(func() {
		restore()
		_ = provider.Shutdown(context.Background())
	})

	f := newLayoutFixture(t)
	f.usage = `"usage":{"input_tokens":40,"output_tokens":5,"total_tokens":45},`
	f.script = func(_ string, body string) string {
		if strings.Contains(body, "function_call_output") {
			return ""
		}
		arguments, _ := json.Marshal(map[string]string{"name": "hello"})
		item, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call-read-1", "name": "skill", "arguments": string(arguments)})
		return string(item)
	}
	if err := os.WriteFile(filepath.Join(f.global, "tools", "coding", "none.json"), []byte(`{"native":{"skill":{}},"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, f.global, "hello", "top secret file body")
	runner := headlessFor(t, f)
	result, err := runner.Run(context.Background(), headless.Request{Session: "local:demo", Profile: "dev/coder", Input: "read hello.txt"})
	if err != nil || result.Status != headless.StatusCompleted {
		t.Fatalf("Run: %+v %v", result, err)
	}

	// Only this run's trace counts: a span from any other run in the process is ignored.
	var run, read tracetest.SpanStub
	var chats []tracetest.SpanStub
	for _, span := range exporter.GetSpans() {
		for _, kv := range span.Attributes {
			if span.Name == "jad.run" && kv.Key == "jad.turn_id" && kv.Value.AsString() == result.TurnID {
				run = span
			}
		}
	}
	for _, span := range exporter.GetSpans() {
		if span.SpanContext.TraceID() != run.SpanContext.TraceID() {
			continue
		}
		switch span.Name {
		case "chat model-small":
			chats = append(chats, span)
		case "execute_tool skill":
			read = span
		}
		for _, kv := range span.Attributes {
			if strings.Contains(kv.Value.Emit(), "top secret") || strings.Contains(kv.Value.Emit(), "read hello.txt") {
				t.Fatalf("span %s carries content in %s", span.Name, kv.Key)
			}
		}
	}
	if run.Name == "" || len(chats) != 2 || read.Name == "" {
		t.Fatalf("spans: run %q, %d chat, tool %q; all: %v", run.Name, len(chats), read.Name, exporter.GetSpans())
	}
	for _, child := range append(chats, read) {
		if child.Parent.SpanID() != run.SpanContext.SpanID() {
			t.Fatalf("%s is not a child of the run span", child.Name)
		}
	}
	values := map[string]string{}
	for _, kv := range run.Attributes {
		values[string(kv.Key)] = kv.Value.Emit()
	}
	if values["jad.session"] != "demo" || values["jad.profile"] != "dev/coder" || values["jad.run.status"] != "completed" || values["jad.turn_id"] != result.TurnID {
		t.Fatalf("run attributes = %v", values)
	}
	tokens := map[string]int64{}
	for _, chat := range chats {
		for _, kv := range chat.Attributes {
			if kv.Key == "gen_ai.usage.input_tokens" {
				tokens[chat.SpanContext.SpanID().String()] = kv.Value.AsInt64()
			}
		}
	}
	if len(tokens) != 2 {
		t.Fatalf("token usage on the chat spans = %v", tokens)
	}
}
