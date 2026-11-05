package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestMetricsSettingsDefaultIsOffAndEveryKeyIsRead(t *testing.T) {
	settings, err := normalizeServerSettings(nil, "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Metrics.Enabled || !reflect.DeepEqual(settings.Metrics, DefaultMetricsSettings()) || len(settings.Metrics.DurationBuckets) != 14 {
		t.Fatalf("default metrics = %+v", settings.Metrics)
	}
	settings, err = normalizeServerSettings(json.RawMessage(`{"metrics":{"enabled":true,"endpoint":" https://otel.example:4318/base ","service_name":"jad-ci","export_interval_ms":5000,"export_timeout_ms":300,"duration_buckets_s":[0.5,1],"token_buckets":[10]}}`), "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	want := MetricsSettings{Enabled: true, Endpoint: "https://otel.example:4318/base", ServiceName: "jad-ci", ExportIntervalMS: 5000, ExportTimeoutMS: 300, DurationBuckets: []float64{0.5, 1}, TokenBuckets: []float64{10}}
	if !reflect.DeepEqual(settings.Metrics, want) {
		t.Fatalf("metrics = %+v", settings.Metrics)
	}
}

func TestMetricsSettingsRefuseBadValues(t *testing.T) {
	for raw, want := range map[string]string{
		`[]`:                           "server.metrics must be an object",
		`{"interval":1}`:               "server.metrics.interval is not a setting",
		`{"enabled":1}`:                "enabled must be a boolean",
		`{"endpoint":"ftp://x"}`:       "endpoint must be an http(s) URL",
		`{"endpoint":"http://u:p@x"}`:  "endpoint must be an http(s) URL",
		`{"service_name":" "}`:         "service_name must be a non-empty string",
		`{"export_interval_ms":999}`:   "export_interval_ms must be an integer between 1000",
		`{"export_timeout_ms":0}`:      "export_timeout_ms must be an integer between 1",
		`{"duration_buckets_s":[]}`:    "duration_buckets_s must be 1 to 64 positive numbers",
		`{"duration_buckets_s":[2,1]}`: "duration_buckets_s must be 1 to 64 positive numbers",
		`{"token_buckets":[0,1]}`:      "token_buckets must be 1 to 64 positive numbers",
		`{"token_buckets":["a"]}`:      "token_buckets must be 1 to 64 positive numbers",
	} {
		if _, err := normalizeServerSettings(json.RawMessage(`{"metrics":`+raw+`}`), "setting.json", "/base"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", raw, err, want)
		}
	}
}

func TestMetricsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/root-setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				AdditionalProperties bool                      `json:"additionalProperties"`
				Properties           map[string]map[string]any `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	metrics := schema.Properties["server"].Properties["metrics"]
	if len(metrics.Properties) != len(metricsKeys) || metrics.AdditionalProperties {
		t.Fatalf("schema keys = %d, loader keys = %v", len(metrics.Properties), metricsKeys)
	}
	defaults := DefaultMetricsSettings()
	asAny := func(values []float64) []any {
		out := make([]any, len(values))
		for index, value := range values {
			out[index] = value
		}
		return out
	}
	for key, want := range map[string]any{
		"enabled": false, "service_name": defaults.ServiceName, "export_interval_ms": float64(defaults.ExportIntervalMS), "export_timeout_ms": float64(defaults.ExportTimeoutMS),
		"duration_buckets_s": asAny(defaults.DurationBuckets), "token_buckets": asAny(defaults.TokenBuckets),
	} {
		if got := metrics.Properties[key]["default"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema default %v, loader default %v", key, got, want)
		}
	}
	for key, bounds := range map[string][2]float64{"export_interval_ms": {minMetricsExportIntervalMS, maxMetricsExportIntervalMS}, "export_timeout_ms": {1, maxTracingTimeoutMS}} {
		if metrics.Properties[key]["minimum"] != bounds[0] || metrics.Properties[key]["maximum"] != bounds[1] {
			t.Errorf("%s: schema bounds %v..%v, loader %v", key, metrics.Properties[key]["minimum"], metrics.Properties[key]["maximum"], bounds)
		}
	}
	for _, key := range []string{"duration_buckets_s", "token_buckets"} {
		if metrics.Properties[key]["maxItems"] != float64(maxMetricsBuckets) {
			t.Errorf("%s: schema maxItems %v", key, metrics.Properties[key]["maxItems"])
		}
	}
}
