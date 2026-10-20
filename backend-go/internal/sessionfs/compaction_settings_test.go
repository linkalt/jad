package sessionfs

import (
	"encoding/json"
	"os"
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

func TestNormalizeCompactionBackupMinTurns(t *testing.T) {
	settings, err := normalizeCompaction(json.RawMessage(`{"keepbackup":{"min_turns":null}}`), "setting.json")
	if err != nil || settings.BackupMinTurns != 0 {
		t.Fatalf("null min_turns = %+v, err=%v, want default 0", settings, err)
	}
	settings, err = normalizeCompaction(json.RawMessage(`{"keepbackup":{"min_turns":1000}}`), "setting.json")
	if err != nil || settings.BackupMinTurns != 1000 {
		t.Fatalf("min_turns 1000 = %+v, err=%v", settings, err)
	}
	for _, raw := range []string{`-1`, `1001`, `1.5`, `"3"`} {
		if _, err := normalizeCompaction(json.RawMessage(`{"keepbackup":{"min_turns":`+raw+`}}`), "setting.json"); err == nil {
			t.Errorf("min_turns %s accepted, want validation error", raw)
		}
	}
}

func TestBackupMinTurnsSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Compaction struct {
				Properties struct {
					KeepBackup struct {
						Properties map[string]struct {
							Type    string `json:"type"`
							Default int    `json:"default"`
							Minimum *int   `json:"minimum"`
							Maximum *int   `json:"maximum"`
						} `json:"properties"`
					} `json:"keepbackup"`
				} `json:"properties"`
			} `json:"compaction"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	field, ok := schema.Properties.Compaction.Properties.KeepBackup.Properties["min_turns"]
	if !ok {
		t.Fatal("setting.schema.json has no compaction.keepbackup.min_turns")
	}
	settings, err := normalizeCompaction(nil, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if field.Type != "integer" || field.Default != settings.BackupMinTurns || field.Minimum == nil || *field.Minimum != 0 || field.Maximum == nil || *field.Maximum != maxKeepBackupTurns {
		t.Fatalf("schema min_turns = %+v, loader default %d max %d", field, settings.BackupMinTurns, maxKeepBackupTurns)
	}
}
