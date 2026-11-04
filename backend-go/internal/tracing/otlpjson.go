package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// The exporter speaks OTLP/HTTP with the JSON encoding (OTLP 1.x, "OTLP/HTTP JSON"):
// one POST of an ExportTraceServiceRequest per batch. The protobuf exporter of the
// OpenTelemetry project links grpc and protobuf, whose reflection keeps every exported
// method of the binary alive and doubled the size of jad; see DECISIONS.md.

// Endpoint variables, as the OpenTelemetry SDKs read them. The traces variable is the full
// URL; the general one is a base to which /v1/traces is appended.
const (
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesHeaders  = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	envHeaders        = "OTEL_EXPORTER_OTLP_HEADERS"
	defaultEndpoint   = "http://localhost:4318"
)

// errorBodyBytes bounds how much of a refused export's answer is kept in the error.
const errorBodyBytes = 512

// jsonExporter is an sdktrace.SpanExporter that posts OTLP/JSON.
type jsonExporter struct {
	url     string
	headers http.Header
	client  *http.Client
}

// newJSONExporter builds the exporter for the configured endpoint (empty: from the
// environment); lookup reads the environment.
func newJSONExporter(endpoint string, lookup func(string) string) (*jsonExporter, error) {
	target := tracesURL(endpoint, lookup)
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("the OTLP traces endpoint %q is not an http(s) URL", target)
	}
	headers := http.Header{}
	for _, name := range []string{envHeaders, envTracesHeaders} {
		if err := parseHeaders(lookup(name), headers); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	headers.Set("Content-Type", "application/json")
	return &jsonExporter{url: target, headers: headers, client: &http.Client{}}, nil
}

func tracesURL(configured string, lookup func(string) string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/") + tracesPath
	}
	if value := strings.TrimSpace(lookup(envTracesEndpoint)); value != "" {
		return value
	}
	if value := strings.TrimSpace(lookup(envEndpoint)); value != "" {
		return strings.TrimRight(value, "/") + tracesPath
	}
	return defaultEndpoint + tracesPath
}

// parseHeaders reads "key1=value1,key2=value2" with URL-encoded values, the format of
// OTEL_EXPORTER_OTLP_HEADERS. A later variable replaces a header an earlier one set.
func parseHeaders(value string, into http.Header) error {
	for _, pair := range strings.Split(value, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		key, raw, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return fmt.Errorf("a header must be key=value")
		}
		decoded, err := url.QueryUnescape(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("header %s: %w", key, err)
		}
		into.Set(key, decoded)
	}
	return nil
}

// ExportSpans posts one batch. The context carries the batch processor's export timeout.
func (e *jsonExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(encodeRequest(spans))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header = e.headers.Clone()
	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("export %d spans: %w", len(spans), err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyBytes))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("export %d spans: the collector answered HTTP %d: %s", len(spans), response.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

// Shutdown releases nothing: each export is one request.
func (e *jsonExporter) Shutdown(context.Context) error { return nil }

// The OTLP/JSON shapes. Ids are lowercase hex and 64-bit integers are decimal strings, as
// the OTLP JSON mapping requires.
type (
	otlpRequest struct {
		ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
	}
	otlpResourceSpans struct {
		Resource   otlpResource     `json:"resource"`
		ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
		SchemaURL  string           `json:"schemaUrl,omitempty"`
	}
	otlpResource struct {
		Attributes []otlpKeyValue `json:"attributes"`
	}
	otlpScopeSpans struct {
		Scope     otlpScope  `json:"scope"`
		Spans     []otlpSpan `json:"spans"`
		SchemaURL string     `json:"schemaUrl,omitempty"`
	}
	otlpScope struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	}
	otlpSpan struct {
		TraceID           string         `json:"traceId"`
		SpanID            string         `json:"spanId"`
		ParentSpanID      string         `json:"parentSpanId,omitempty"`
		Name              string         `json:"name"`
		Kind              int            `json:"kind"`
		StartTimeUnixNano string         `json:"startTimeUnixNano"`
		EndTimeUnixNano   string         `json:"endTimeUnixNano"`
		Attributes        []otlpKeyValue `json:"attributes,omitempty"`
		Events            []otlpEvent    `json:"events,omitempty"`
		Status            otlpStatus     `json:"status"`
	}
	otlpEvent struct {
		TimeUnixNano string         `json:"timeUnixNano"`
		Name         string         `json:"name"`
		Attributes   []otlpKeyValue `json:"attributes,omitempty"`
	}
	otlpStatus struct {
		Code    int    `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
	otlpKeyValue struct {
		Key   string    `json:"key"`
		Value otlpValue `json:"value"`
	}
	otlpValue struct {
		StringValue *string         `json:"stringValue,omitempty"`
		BoolValue   *bool           `json:"boolValue,omitempty"`
		IntValue    *string         `json:"intValue,omitempty"`
		DoubleValue *float64        `json:"doubleValue,omitempty"`
		ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
	}
	otlpArrayValue struct {
		Values []otlpValue `json:"values"`
	}
)

// OTLP span kinds and status codes.
const (
	otlpStatusOK    = 1
	otlpStatusError = 2
)

func encodeRequest(spans []sdktrace.ReadOnlySpan) otlpRequest {
	type scopeKey struct {
		resource *resource.Resource
		scope    instrumentation.Scope
	}
	var request otlpRequest
	resourceIndex := map[*resource.Resource]int{}
	scopeIndex := map[scopeKey]int{}
	for _, span := range spans {
		res := span.Resource()
		r, ok := resourceIndex[res]
		if !ok {
			r = len(request.ResourceSpans)
			resourceIndex[res] = r
			request.ResourceSpans = append(request.ResourceSpans, otlpResourceSpans{Resource: otlpResource{Attributes: encodeAttributes(res.Attributes())}, SchemaURL: res.SchemaURL()})
		}
		key := scopeKey{resource: res, scope: span.InstrumentationScope()}
		s, ok := scopeIndex[key]
		if !ok {
			s = len(request.ResourceSpans[r].ScopeSpans)
			scopeIndex[key] = s
			request.ResourceSpans[r].ScopeSpans = append(request.ResourceSpans[r].ScopeSpans, otlpScopeSpans{
				Scope: otlpScope{Name: key.scope.Name, Version: key.scope.Version}, SchemaURL: key.scope.SchemaURL,
			})
		}
		scope := &request.ResourceSpans[r].ScopeSpans[s]
		scope.Spans = append(scope.Spans, encodeSpan(span))
	}
	return request
}

func encodeSpan(span sdktrace.ReadOnlySpan) otlpSpan {
	context := span.SpanContext()
	encoded := otlpSpan{
		TraceID:           hexTraceID(context.TraceID()),
		SpanID:            hexSpanID(context.SpanID()),
		Name:              span.Name(),
		Kind:              otlpKind(span.SpanKind()),
		StartTimeUnixNano: strconv.FormatInt(span.StartTime().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(span.EndTime().UnixNano(), 10),
		Attributes:        encodeAttributes(span.Attributes()),
	}
	if parent := span.Parent(); parent.SpanID().IsValid() {
		encoded.ParentSpanID = hexSpanID(parent.SpanID())
	}
	switch status := span.Status(); status.Code {
	case codes.Ok:
		encoded.Status = otlpStatus{Code: otlpStatusOK}
	case codes.Error:
		encoded.Status = otlpStatus{Code: otlpStatusError, Message: status.Description}
	}
	for _, event := range span.Events() {
		encoded.Events = append(encoded.Events, otlpEvent{
			TimeUnixNano: strconv.FormatInt(event.Time.UnixNano(), 10),
			Name:         event.Name,
			Attributes:   encodeAttributes(event.Attributes),
		})
	}
	return encoded
}

// otlpKind maps a span kind to the OTLP enum, where 0 is unspecified and the others follow
// the order internal, server, client, producer, consumer: the same numbers trace.SpanKind has.
func otlpKind(kind trace.SpanKind) int {
	if kind < trace.SpanKindInternal || kind > trace.SpanKindConsumer {
		return int(trace.SpanKindInternal)
	}
	return int(kind)
}

func hexTraceID(id trace.TraceID) string { return hex.EncodeToString(id[:]) }
func hexSpanID(id trace.SpanID) string   { return hex.EncodeToString(id[:]) }

func encodeAttributes(attributes []attribute.KeyValue) []otlpKeyValue {
	if len(attributes) == 0 {
		return nil
	}
	encoded := make([]otlpKeyValue, 0, len(attributes))
	for _, kv := range attributes {
		encoded = append(encoded, otlpKeyValue{Key: string(kv.Key), Value: encodeValue(kv.Value)})
	}
	return encoded
}

func encodeValue(value attribute.Value) otlpValue {
	switch value.Type() {
	case attribute.BOOL:
		b := value.AsBool()
		return otlpValue{BoolValue: &b}
	case attribute.INT64:
		i := strconv.FormatInt(value.AsInt64(), 10)
		return otlpValue{IntValue: &i}
	case attribute.FLOAT64:
		f := value.AsFloat64()
		return otlpValue{DoubleValue: &f}
	case attribute.BOOLSLICE:
		var values []otlpValue
		for _, b := range value.AsBoolSlice() {
			values = append(values, encodeValue(attribute.BoolValue(b)))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: values}}
	case attribute.INT64SLICE:
		var values []otlpValue
		for _, i := range value.AsInt64Slice() {
			values = append(values, encodeValue(attribute.Int64Value(i)))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: values}}
	case attribute.FLOAT64SLICE:
		var values []otlpValue
		for _, f := range value.AsFloat64Slice() {
			values = append(values, encodeValue(attribute.Float64Value(f)))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: values}}
	case attribute.STRINGSLICE:
		var values []otlpValue
		for _, s := range value.AsStringSlice() {
			values = append(values, encodeValue(attribute.StringValue(s)))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: values}}
	default:
		s := value.Emit()
		return otlpValue{StringValue: &s}
	}
}
