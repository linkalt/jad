package sessionfs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Defaults and bounds of server.metrics. The schema (root-setting.schema.json) states the
// same values.
const (
	DefaultMetricsServiceName      = "jad"
	DefaultMetricsExportIntervalMS = 60_000
	DefaultMetricsExportTimeoutMS  = 10_000
	minMetricsExportIntervalMS     = 1_000
	maxMetricsExportIntervalMS     = 60 * 60 * 1000
	maxMetricsBuckets              = 64
)

var (
	metricsKeys = []string{"enabled", "endpoint", "service_name", "export_interval_ms", "export_timeout_ms", "duration_buckets_s", "token_buckets"}
	// The bucket boundaries the OpenTelemetry GenAI conventions advise for
	// gen_ai.client.operation.duration and gen_ai.client.token.usage.
	defaultMetricsDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	defaultMetricsTokenBuckets    = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
)

// MetricsSettings is server.metrics: OpenTelemetry metrics of runs, model requests and tool
// calls (counts, durations, token usage), pushed over OTLP/HTTP JSON on a fixed interval.
// Like spans, they carry model and tool names and outcomes, never session ids, prompts,
// answers, tool arguments or tool output.
type MetricsSettings struct {
	Enabled bool
	// Endpoint is the OTLP/HTTP base URL; metrics go to <endpoint>/v1/metrics. Empty leaves
	// it to OTEL_EXPORTER_OTLP_METRICS_ENDPOINT and OTEL_EXPORTER_OTLP_ENDPOINT, else
	// http://localhost:4318.
	Endpoint         string
	ServiceName      string
	ExportIntervalMS int64
	ExportTimeoutMS  int64
	// DurationBuckets and TokenBuckets are the explicit histogram bucket boundaries, in
	// seconds and tokens, increasing.
	DurationBuckets []float64
	TokenBuckets    []float64
}

// DefaultMetricsSettings is server.metrics when it is absent: off.
func DefaultMetricsSettings() MetricsSettings {
	return MetricsSettings{
		ServiceName:      DefaultMetricsServiceName,
		ExportIntervalMS: DefaultMetricsExportIntervalMS,
		ExportTimeoutMS:  DefaultMetricsExportTimeoutMS,
		DurationBuckets:  slices.Clone(defaultMetricsDurationBuckets),
		TokenBuckets:     slices.Clone(defaultMetricsTokenBuckets),
	}
}

func normalizeMetrics(raw json.RawMessage, path string) (MetricsSettings, error) {
	settings := DefaultMetricsSettings()
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return MetricsSettings{}, fmt.Errorf("%s: server.metrics must be an object", path)
	}
	for key := range fields {
		if !slices.Contains(metricsKeys, key) {
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.%s is not a setting (%s)", path, key, strings.Join(metricsKeys, ", "))
		}
	}
	if value, ok := fields["enabled"]; ok && string(value) != "null" {
		if json.Unmarshal(value, &settings.Enabled) != nil {
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.enabled must be a boolean", path)
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
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.%s must be a non-empty string", path, field.name)
		}
		*field.target = strings.TrimSpace(parsed)
	}
	if settings.Endpoint != "" {
		endpoint, err := url.Parse(settings.Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.endpoint must be an http(s) URL without credentials, query or fragment", path)
		}
	}
	for _, field := range []struct {
		name     string
		target   *int64
		min, max int64
	}{
		{"export_interval_ms", &settings.ExportIntervalMS, minMetricsExportIntervalMS, maxMetricsExportIntervalMS},
		{"export_timeout_ms", &settings.ExportTimeoutMS, 1, maxTracingTimeoutMS},
	} {
		value, ok := fields[field.name]
		if !ok || string(value) == "null" {
			continue
		}
		parsed, valid := configSafeInteger(value)
		if !valid || parsed < field.min || parsed > field.max {
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.%s must be an integer between %d and %d", path, field.name, field.min, field.max)
		}
		*field.target = parsed
	}
	for _, field := range []struct {
		name   string
		target *[]float64
	}{{"duration_buckets_s", &settings.DurationBuckets}, {"token_buckets", &settings.TokenBuckets}} {
		value, ok := fields[field.name]
		if !ok || string(value) == "null" {
			continue
		}
		var buckets []float64
		if json.Unmarshal(value, &buckets) != nil || len(buckets) == 0 || len(buckets) > maxMetricsBuckets || !strictlyIncreasing(buckets) {
			return MetricsSettings{}, fmt.Errorf("%s: server.metrics.%s must be 1 to %d positive numbers in increasing order", path, field.name, maxMetricsBuckets)
		}
		*field.target = buckets
	}
	return settings, nil
}

func strictlyIncreasing(values []float64) bool {
	for index, value := range values {
		if value <= 0 || (index > 0 && value <= values[index-1]) {
			return false
		}
	}
	return true
}
