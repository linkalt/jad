package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTracingSettingsDefaultsAndValues(t *testing.T) {
	settings, err := normalizeServerSettings(json.RawMessage(`{}`), "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	want := TracingSettings{ServiceName: "jad", SampleRatio: 1, ExportTimeoutMS: 10_000, BatchTimeoutMS: 5_000, MaxQueueSize: 2048}
	if settings.Tracing != want || Defaults().Tracing != want {
		t.Fatalf("default tracing = %+v / %+v, want %+v", settings.Tracing, Defaults().Tracing, want)
	}
	settings, err = normalizeServerSettings(json.RawMessage(`{"tracing":{"enabled":true,"endpoint":" https://otel.example:4318/base ","service_name":"jad-ci","sample_ratio":0.25,"export_timeout_ms":3000,"batch_timeout_ms":100,"max_queue_size":64}}`), "setting.json", "/base")
	if err != nil {
		t.Fatal(err)
	}
	want = TracingSettings{Enabled: true, Endpoint: "https://otel.example:4318/base", ServiceName: "jad-ci", SampleRatio: 0.25, ExportTimeoutMS: 3000, BatchTimeoutMS: 100, MaxQueueSize: 64}
	if settings.Tracing != want {
		t.Fatalf("tracing = %+v, want %+v", settings.Tracing, want)
	}
}

func TestTracingSettingsRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"not an object":     `"on"`,
		"unknown key":       `{"headers":{"a":"b"}}`,
		"enabled type":      `{"enabled":"yes"}`,
		"endpoint scheme":   `{"endpoint":"grpc://collector:4317"}`,
		"endpoint userinfo": `{"endpoint":"http://user:pw@collector:4318"}`,
		"endpoint query":    `{"endpoint":"http://collector:4318?x=1"}`,
		"endpoint empty":    `{"endpoint":" "}`,
		"service name type": `{"service_name":1}`,
		"ratio above one":   `{"sample_ratio":1.5}`,
		"ratio negative":    `{"sample_ratio":-0.1}`,
		"timeout zero":      `{"export_timeout_ms":0}`,
		"timeout too large": `{"batch_timeout_ms":300001}`,
		"queue not integer": `{"max_queue_size":1.5}`,
		"queue too large":   `{"max_queue_size":1048577}`,
	} {
		if _, err := normalizeServerSettings(json.RawMessage(`{"tracing":`+raw+`}`), "setting.json", "/base"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The schema states the loader's defaults and bounds, and lists the loader's keys.
func TestTracingSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "root-setting.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Default any `json:"default"`
					Maximum any `json:"maximum"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	tracing := schema.Properties["server"].Properties["tracing"]
	keys := []string{}
	for key := range tracing.Properties {
		keys = append(keys, key)
	}
	if len(keys) != len(tracingKeys) || tracing.AdditionalProperties {
		t.Fatalf("schema keys = %v, loader keys = %v", keys, tracingKeys)
	}
	defaults := defaultTracingSettings()
	for key, want := range map[string]any{
		"enabled": false, "service_name": defaults.ServiceName, "sample_ratio": defaults.SampleRatio,
		"export_timeout_ms": float64(defaults.ExportTimeoutMS), "batch_timeout_ms": float64(defaults.BatchTimeoutMS), "max_queue_size": float64(defaults.MaxQueueSize),
	} {
		if got := tracing.Properties[key].Default; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema default %v, loader default %v", key, got, want)
		}
	}
	for key, want := range map[string]float64{"export_timeout_ms": maxTracingTimeoutMS, "batch_timeout_ms": maxTracingTimeoutMS, "max_queue_size": maxTracingQueueSize} {
		if got := tracing.Properties[key].Maximum; got != want {
			t.Errorf("%s: schema maximum %v, loader maximum %v", key, got, want)
		}
	}
}
