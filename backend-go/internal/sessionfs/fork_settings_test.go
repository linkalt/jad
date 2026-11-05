package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"

	"jad/backend-go/internal/serverconfig"
)

func TestServerForkNameMaxBytesDefaultValuesAndRefusals(t *testing.T) {
	settings, err := normalizeServerSettings(json.RawMessage(`{}`), "setting.json", t.TempDir())
	if err != nil || settings.ForkNameMaxBytes != 200 || serverconfig.DefaultForkNameMaxBytes != 200 {
		t.Fatalf("default fork_name_max_bytes = %d, %v", settings.ForkNameMaxBytes, err)
	}
	for value, want := range map[string]int{"1": 1, "250": 250, "null": 200} {
		settings, err := normalizeServerSettings(json.RawMessage(`{"fork_name_max_bytes":`+value+`}`), "setting.json", t.TempDir())
		if err != nil || settings.ForkNameMaxBytes != want {
			t.Fatalf("fork_name_max_bytes %s = %d, %v; want %d", value, settings.ForkNameMaxBytes, err, want)
		}
	}
	for _, value := range []string{"0", "251", "-1", "1.5", `"200"`} {
		if _, err := normalizeServerSettings(json.RawMessage(`{"fork_name_max_bytes":`+value+`}`), "setting.json", t.TempDir()); err == nil || !strings.Contains(err.Error(), "fork_name_max_bytes") {
			t.Errorf("fork_name_max_bytes %s error = %v, want a refusal naming the key", value, err)
		}
	}
	schema := compileSchema(t, "root-setting.schema.json#/properties/server/properties/fork_name_max_bytes")
	for raw, accept := range map[string]bool{"1": true, "250": true, "0": false, "251": false, "1.5": false} {
		if got := schemaAccepts(t, schema, raw); got != accept {
			t.Errorf("schema accepts %s = %v, want %v", raw, got, accept)
		}
	}
}
