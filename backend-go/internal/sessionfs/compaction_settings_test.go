package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeCompactionUsesTypeScriptDefaultsForNullValues(t *testing.T) {
	settings, err := normalizeCompaction(json.RawMessage(`{"auto_enabled":null,"trigger_tokens":null,"backup_mode":null,"keepbatch":{"tools":null,"turn":{"type":null,"keep":null}},"keepbackup":null}`), "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.AutoEnabled || settings.TriggerTokens != defaultCompactionTriggerTokens || settings.TurnType != "assistant_final" || settings.KeepTurns != 1 || settings.KeepToolPairs != 0 || settings.BackupMode != "append" || settings.KeepBackupTurns != defaultKeepBackupTurns {
		t.Fatalf("normalized compaction defaults = %+v", settings)
	}
}

func TestNormalizeCompactionReadsManualSettings(t *testing.T) {
	settings, err := normalizeCompaction(json.RawMessage(`{"auto_enabled":false,"trigger_tokens":1234,"backup_mode":"replace","keepbatch":{"tools":2,"turn":{"type":"full","keep":3}},"keepbackup":{"turns":4}}`), "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if settings.AutoEnabled || settings.TriggerTokens != 1234 || settings.TurnType != "full" || settings.KeepTurns != 3 || settings.KeepToolPairs != 2 || settings.BackupMode != "replace" || settings.KeepBackupTurns != 4 {
		t.Fatalf("normalized compaction settings = %+v", settings)
	}
}

func TestNormalizeCompactionRejectsInvalidAutomaticSettings(t *testing.T) {
	for _, raw := range []string{
		`{"auto_enabled":"true"}`,
		`{"trigger_tokens":0}`,
		`{"trigger_tokens":10000001}`,
		`{"trigger_tokens":1.5}`,
		`{"trigger_tokens":"200000"}`,
	} {
		if _, err := normalizeCompaction(json.RawMessage(raw), "setting.json"); err == nil {
			t.Errorf("normalizeCompaction(%s) succeeded, want validation error", raw)
		}
	}
}

func TestNormalizeCompactionAppendLimit(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{``, 0},
		{`{}`, 0},
		{`{"keepbackup":null}`, 0},
		{`{"keepbackup":{}}`, 0},
		{`{"keepbackup":{"append_limit":null}}`, 0},
		{`{"keepbackup":{"append_limit":0}}`, 0},
		{`{"keepbackup":{"append_limit":7}}`, 7},
		{`{"keepbackup":{"append_limit":1000}}`, 1000},
	} {
		settings, err := normalizeCompaction(json.RawMessage(tc.raw), "setting.json")
		if err != nil || settings.AppendLimit != tc.want {
			t.Errorf("%q: append_limit = %d, err=%v; want %d", tc.raw, settings.AppendLimit, err, tc.want)
		}
	}
	for _, raw := range []string{`-1`, `1001`, `1.5`, `"3"`, `true`, `[]`, `1e400`} {
		_, err := normalizeCompaction(json.RawMessage(`{"keepbackup":{"append_limit":`+raw+`}}`), "setting.json")
		if err == nil || !strings.Contains(err.Error(), "compaction.keepbackup.append_limit must be an integer between 0 and 1000") {
			t.Errorf("append_limit %s: err = %v, want the range error", raw, err)
		}
	}
	settings, err := normalizeCompaction(json.RawMessage(`{"keepbackup":{"turns":4,"append_limit":9}}`), "setting.json")
	if err != nil || settings.KeepBackupTurns != 4 || settings.AppendLimit != 9 {
		t.Fatalf("turns and append_limit = %+v, err=%v; they are independent", settings, err)
	}
}

// The settings that were removed are ignored, not applied and not refused: a setting.json
// written for them still loads.
func TestNormalizeCompactionIgnoresRemovedSummaryAndMinTurns(t *testing.T) {
	want, err := normalizeCompaction(nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"keepbackup":{"min_turns":5},"summary":{"enabled":true,"prompt":"P","role":"nonsense","max_input_bytes":1,"model":"x"}}`
	settings, err := normalizeCompaction(json.RawMessage(raw), "setting.json")
	if err != nil {
		t.Fatalf("a removed key was refused: %v", err)
	}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("removed keys changed the settings: %+v, want the defaults %+v", settings, want)
	}
	settings, err = normalizeCompaction(json.RawMessage(`{"backup_mode":"replace","keepbackup":{"min_turns":5,"append_limit":3}}`), "setting.json")
	if err != nil || settings.AppendLimit != 3 || settings.BackupMode != "replace" {
		t.Fatalf("min_turns beside append_limit = %+v, err=%v", settings, err)
	}
}

func TestAppendLimitSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Compaction struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"compaction"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties.Compaction.Properties["summary"]; ok {
		t.Error("setting.schema.json still has compaction.summary")
	}
	var keepbackup struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Default int    `json:"default"`
			Minimum *int   `json:"minimum"`
			Maximum *int   `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Properties.Compaction.Properties["keepbackup"], &keepbackup); err != nil {
		t.Fatal(err)
	}
	if _, ok := keepbackup.Properties["min_turns"]; ok {
		t.Error("setting.schema.json still has compaction.keepbackup.min_turns")
	}
	field, ok := keepbackup.Properties["append_limit"]
	if !ok {
		t.Fatal("setting.schema.json has no compaction.keepbackup.append_limit")
	}
	settings, err := normalizeCompaction(nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if field.Type != "integer" || field.Default != settings.AppendLimit || field.Minimum == nil || *field.Minimum != 0 || field.Maximum == nil || *field.Maximum != maxKeepBackupTurns {
		t.Fatalf("schema append_limit = %+v, loader default %d max %d", field, settings.AppendLimit, maxKeepBackupTurns)
	}
}
