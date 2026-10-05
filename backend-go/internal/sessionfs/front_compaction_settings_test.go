package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeFrontCompactionDefaultsAndNullMembers(t *testing.T) {
	encoded, settings, err := normalizeFrontCompaction(nil, false, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.DeleteMode != "permanent" {
		t.Fatalf("default front compaction settings = %+v", settings)
	}
	if !json.Valid(encoded) || !strings.Contains(string(encoded), `"enabled":true`) || !strings.Contains(string(encoded), `"delete_mode":"permanent"`) {
		t.Fatalf("normalized default front_compaction = %s", encoded)
	}

	_, settings, err = normalizeFrontCompaction(json.RawMessage(`{"enabled":null,"delete_mode":null}`), true, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.DeleteMode != "permanent" {
		t.Fatalf("null member defaults = %+v", settings)
	}
}

func TestNormalizeFrontCompactionPreservesRecoverableSettings(t *testing.T) {
	encoded, settings, err := normalizeFrontCompaction(json.RawMessage(`{"enabled":false,"delete_mode":"recoverable","custom":1}`), true, "setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled || settings.DeleteMode != "recoverable" || !strings.Contains(string(encoded), `"custom":1`) {
		t.Fatalf("normalized front_compaction = %s settings=%+v", encoded, settings)
	}
}

func TestNormalizeFrontCompactionRejectsInvalidValues(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{"enabled":"yes"}`), json.RawMessage(`{"delete_mode":"unknown"}`)} {
		if _, _, err := normalizeFrontCompaction(raw, true, "setting.json"); err == nil {
			t.Errorf("normalizeFrontCompaction(%s) unexpectedly succeeded", raw)
		}
	}
}
