package sessionfs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Defaults and bounds of server.tracing. The schema (root-setting.schema.json) states the
// same values.
const (
	DefaultTracingServiceName     = "jad"
	DefaultTracingSampleRatio     = 1.0
	DefaultTracingExportTimeoutMS = 10_000
	DefaultTracingBatchTimeoutMS  = 5_000
	DefaultTracingMaxQueueSize    = 2048
	maxTracingTimeoutMS           = 5 * 60 * 1000
	maxTracingQueueSize           = 1 << 20
)

var tracingKeys = []string{"enabled", "endpoint", "service_name", "sample_ratio", "export_timeout_ms", "batch_timeout_ms", "max_queue_size"}

// TracingSettings is server.tracing: OpenTelemetry spans for runs, model requests and tool
// calls, exported over OTLP/HTTP. Spans carry names, ids, models, token counts and
// outcomes; never prompts, answers, tool arguments or tool output.
type TracingSettings struct {
	Enabled bool
	// Endpoint is the OTLP/HTTP base URL (for example http://127.0.0.1:4318); spans go to
	// <endpoint>/v1/traces. Empty leaves it to the OTEL_EXPORTER_OTLP_TRACES_ENDPOINT and
	// OTEL_EXPORTER_OTLP_ENDPOINT variables, else the exporter's default
	// (http://localhost:4318).
	Endpoint        string
	ServiceName     string
	SampleRatio     float64
	ExportTimeoutMS int64
	BatchTimeoutMS  int64
	MaxQueueSize    int64
}

func defaultTracingSettings() TracingSettings {
	return TracingSettings{
		ServiceName:     DefaultTracingServiceName,
		SampleRatio:     DefaultTracingSampleRatio,
		ExportTimeoutMS: DefaultTracingExportTimeoutMS,
		BatchTimeoutMS:  DefaultTracingBatchTimeoutMS,
		MaxQueueSize:    DefaultTracingMaxQueueSize,
	}
}

func normalizeTracing(raw json.RawMessage, path string) (TracingSettings, error) {
	settings := defaultTracingSettings()
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return TracingSettings{}, fmt.Errorf("%s: server.tracing must be an object", path)
	}
	for key := range fields {
		if !containsString(tracingKeys, key) {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.%s is not a setting (%s)", path, key, strings.Join(tracingKeys, ", "))
		}
	}
	if value, ok := fields["enabled"]; ok && string(value) != "null" {
		if json.Unmarshal(value, &settings.Enabled) != nil {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.enabled must be a boolean", path)
		}
	}
	for _, field := range []struct {
		name   string
		target *string
	}{{"endpoint", &settings.Endpoint}, {"service_name", &settings.ServiceName}} {
		value, ok := fields[field.name]
		if !ok || string(value) == "null" {
			continue
		}
		var parsed string
		if json.Unmarshal(value, &parsed) != nil || strings.TrimSpace(parsed) == "" {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.%s must be a non-empty string", path, field.name)
		}
		*field.target = strings.TrimSpace(parsed)
	}
	if settings.Endpoint != "" {
		endpoint, err := url.Parse(settings.Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.endpoint must be an http(s) URL without credentials, query or fragment", path)
		}
	}
	if value, ok := fields["sample_ratio"]; ok && string(value) != "null" {
		var ratio float64
		if json.Unmarshal(value, &ratio) != nil || ratio < 0 || ratio > 1 {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.sample_ratio must be a number between 0 and 1", path)
		}
		settings.SampleRatio = ratio
	}
	for _, field := range []struct {
		name     string
		target   *int64
		min, max int64
	}{
		{"export_timeout_ms", &settings.ExportTimeoutMS, 1, maxTracingTimeoutMS},
		{"batch_timeout_ms", &settings.BatchTimeoutMS, 1, maxTracingTimeoutMS},
		{"max_queue_size", &settings.MaxQueueSize, 1, maxTracingQueueSize},
	} {
		value, ok := fields[field.name]
		if !ok || string(value) == "null" {
			continue
		}
		parsed, valid := configSafeInteger(value)
		if !valid || parsed < field.min || parsed > field.max {
			return TracingSettings{}, fmt.Errorf("%s: server.tracing.%s must be an integer between %d and %d", path, field.name, field.min, field.max)
		}
		*field.target = parsed
	}
	return settings, nil
}
