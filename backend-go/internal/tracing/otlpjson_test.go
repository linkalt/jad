package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"jad/backend-go/internal/sessionfs"
)

func lookupFrom(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestTracesURLPrecedence(t *testing.T) {
	for name, test := range map[string]struct {
		configured string
		env        map[string]string
		want       string
	}{
		"configured wins":       {"http://a:4318/", map[string]string{envTracesEndpoint: "http://b/v1/traces"}, "http://a:4318/v1/traces"},
		"traces variable as is": {"", map[string]string{envTracesEndpoint: "http://b:9/custom", envEndpoint: "http://c"}, "http://b:9/custom"},
		"general variable base": {"", map[string]string{envEndpoint: "https://c:4318/"}, "https://c:4318/v1/traces"},
		"default":               {"", nil, "http://localhost:4318/v1/traces"},
	} {
		if got := tracesURL(test.configured, lookupFrom(test.env)); got != test.want {
			t.Errorf("%s: %q, want %q", name, got, test.want)
		}
	}
}

func TestExporterHeadersFromTheEnvironment(t *testing.T) {
	exporter, err := newJSONExporter("http://collector", lookupFrom(map[string]string{
		envHeaders:       "x-team=core, authorization=Basic%20abc",
		envTracesHeaders: "x-team=traces",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if exporter.headers.Get("Authorization") != "Basic abc" || exporter.headers.Get("X-Team") != "traces" || exporter.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", exporter.headers)
	}
	for _, bad := range []string{"novalue", "=x", "k=%zz"} {
		if _, err := newJSONExporter("http://collector", lookupFrom(map[string]string{envHeaders: bad})); err == nil {
			t.Errorf("headers %q: no error", bad)
		}
	}
	if _, err := newJSONExporter("", lookupFrom(map[string]string{envTracesEndpoint: "collector:4318"})); err == nil {
		t.Error("an endpoint without a scheme: no error")
	}
}

// collector is an OTLP/HTTP receiver that keeps each request body.
type collector struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	status  int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies, c.headers, c.paths = append(c.bodies, body), append(c.headers, r.Header.Clone()), append(c.paths, r.URL.Path)
	status := c.status
	c.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "quota exceeded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func TestExporterPostsOTLPJSON(t *testing.T) {
	receiver := &collector{}
	server := httptest.NewServer(receiver)
	t.Cleanup(server.Close)
	settings := sessionfs.TracingSettings{Enabled: true, Endpoint: server.URL, ServiceName: "jad-test", SampleRatio: 1, ExportTimeoutMS: 5000, BatchTimeoutMS: 60_000, MaxQueueSize: 16}
	provider, err := NewProvider(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	restore := Install(provider)
	ctx, run := StartRun(context.Background(), "local:demo", "dev/coder", "web-1", "")
	_, model := StartModelRequest(ctx, "gpt-x", 1)
	EndModelRequest(model, json.RawMessage(`{"response":{"id":"r","status":"completed","usage":{"input_tokens":12345678901,"output_tokens":2}}}`), nil)
	EndRun(run, "failed", errors.New("x"))
	// The batch timeout is a minute: only the shutdown flush sends the spans.
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restore()

	if len(receiver.bodies) != 1 || receiver.paths[0] != "/v1/traces" || receiver.headers[0].Get("Content-Type") != "application/json" {
		t.Fatalf("requests = %d, paths %v", len(receiver.bodies), receiver.paths)
	}
	var request struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope struct {
					Name string `json:"name"`
				} `json:"scope"`
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Name         string `json:"name"`
					Kind         int    `json:"kind"`
					Start        string `json:"startTimeUnixNano"`
					End          string `json:"endTimeUnixNano"`
					Attributes   []struct {
						Key   string                     `json:"key"`
						Value map[string]json.RawMessage `json:"value"`
					} `json:"attributes"`
					Status struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(receiver.bodies[0], &request); err != nil {
		t.Fatalf("body is not OTLP JSON: %v\n%s", err, receiver.bodies[0])
	}
	if len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("grouping: %s", receiver.bodies[0])
	}
	service := ""
	for _, kv := range request.ResourceSpans[0].Resource.Attributes {
		if kv.Key == "service.name" {
			service = kv.Value.StringValue
		}
	}
	scope := request.ResourceSpans[0].ScopeSpans[0]
	if service != "jad-test" || scope.Scope.Name != instrumentationName || len(scope.Spans) != 2 {
		t.Fatalf("service %q, scope %q, spans %d", service, scope.Scope.Name, len(scope.Spans))
	}
	chat, root := scope.Spans[0], scope.Spans[1]
	if root.Name != "jad.run" || chat.Name != "chat gpt-x" {
		t.Fatalf("names %q %q", root.Name, chat.Name)
	}
	if len(root.TraceID) != 32 || len(root.SpanID) != 16 || chat.TraceID != root.TraceID || chat.ParentSpanID != root.SpanID || root.ParentSpanID != "" {
		t.Fatalf("ids: root %s/%s, chat %s/%s parent %s", root.TraceID, root.SpanID, chat.TraceID, chat.SpanID, chat.ParentSpanID)
	}
	if chat.Kind != 3 || root.Kind != 1 || root.Status.Code != 2 || chat.Status.Code != 0 || root.Start == "" || root.End < root.Start {
		t.Fatalf("kind/status/time: chat %d/%d, root %d/%d %s-%s", chat.Kind, chat.Status.Code, root.Kind, root.Status.Code, root.Start, root.End)
	}
	values := map[string]map[string]json.RawMessage{}
	for _, kv := range chat.Attributes {
		values[kv.Key] = kv.Value
	}
	// A 64-bit integer is a decimal string, so a large token count survives JSON numbers.
	if string(values["gen_ai.usage.input_tokens"]["intValue"]) != `"12345678901"` || string(values["jad.round"]["intValue"]) != `"1"` {
		t.Fatalf("integer attributes = %s %s", values["gen_ai.usage.input_tokens"]["intValue"], values["jad.round"]["intValue"])
	}
	if string(values["gen_ai.response.finish_reasons"]["arrayValue"]) != `{"values":[{"stringValue":"completed"}]}` {
		t.Fatalf("array attribute = %s", values["gen_ai.response.finish_reasons"]["arrayValue"])
	}
}

func TestExporterReportsARefusedExport(t *testing.T) {
	receiver := &collector{status: http.StatusTooManyRequests}
	server := httptest.NewServer(receiver)
	t.Cleanup(server.Close)
	exporter, err := newJSONExporter(server.URL, lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	stub := tracetest.SpanStub{
		Name:        "x",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}}),
		StartTime:   time.Unix(1, 0), EndTime: time.Unix(2, 0),
		Attributes: []attribute.KeyValue{attribute.Bool("b", true), attribute.Float64("f", 0.5), attribute.Int64Slice("i", []int64{1})},
		Status:     sdktrace.Status{Code: codes.Error, Description: "boom"},
	}
	err = exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error = %v, want the status and the collector's answer", err)
	}
	if !strings.Contains(string(receiver.bodies[0]), `"traceId":"01000000000000000000000000000000"`) || !strings.Contains(string(receiver.bodies[0]), `"doubleValue":0.5`) {
		t.Fatalf("body = %s", receiver.bodies[0])
	}
	if err := exporter.ExportSpans(context.Background(), nil); err != nil || len(receiver.bodies) != 1 {
		t.Fatalf("an empty batch was sent: %v", err)
	}
}
