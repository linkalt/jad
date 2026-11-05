package sessionfs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/serverconfig"
)

func TestServerSteerMaxPendingDefaultValuesAndRefusals(t *testing.T) {
	settings, err := normalizeServerSettings(nil, "setting.json", t.TempDir())
	if err != nil || settings.SteerMaxPending != serverconfig.DefaultSteerMaxPending {
		t.Fatalf("default steer_max_pending = %d, %v", settings.SteerMaxPending, err)
	}
	for _, value := range []string{"0", "1000", "null"} {
		settings, err := normalizeServerSettings(json.RawMessage(`{"steer_max_pending":`+value+`}`), "setting.json", t.TempDir())
		if err != nil {
			t.Fatalf("steer_max_pending %s: %v", value, err)
		}
		want := map[string]int{"0": 0, "1000": 1000, "null": serverconfig.DefaultSteerMaxPending}[value]
		if settings.SteerMaxPending != want {
			t.Fatalf("steer_max_pending %s = %d, want %d", value, settings.SteerMaxPending, want)
		}
	}
	for _, value := range []string{"-1", "1001", "1.5", `"4"`} {
		if _, err := normalizeServerSettings(json.RawMessage(`{"steer_max_pending":`+value+`}`), "setting.json", t.TempDir()); err == nil || !strings.Contains(err.Error(), "steer_max_pending") {
			t.Errorf("steer_max_pending %s error = %v, want a refusal naming the key", value, err)
		}
	}
}

func TestServerSteerMaxPendingSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/root-setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Server struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"server"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	raw, ok := schema.Properties.Server.Properties["steer_max_pending"]
	var field struct {
		Default *int `json:"default"`
		Minimum *int `json:"minimum"`
		Maximum *int `json:"maximum"`
	}
	if ok {
		if err := json.Unmarshal(raw, &field); err != nil {
			t.Fatal(err)
		}
	}
	if !ok || field.Default == nil || *field.Default != serverconfig.DefaultSteerMaxPending || field.Minimum == nil || *field.Minimum != 0 || field.Maximum == nil || *field.Maximum != serverconfig.MaxSteerMaxPending {
		t.Fatalf("schema server.steer_max_pending = %+v", field)
	}
}
