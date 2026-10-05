package sessionfs

import (
	"encoding/json"
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
