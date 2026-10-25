package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func transcriptsSettingsFor(body string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"transcripts":` + body + `}}`)}
}

func TestTranscriptsSettingsAreOffUnlessConfigured(t *testing.T) {
	settings, err := normalizeNativeTranscriptsSettings(map[string]json.RawMessage{}, "s.json", NativePathPolicy{})
	if err != nil || settings != nil {
		t.Fatalf("settings = %v, err = %v", settings, err)
	}
}

func TestTranscriptsSettingsDefaults(t *testing.T) {
	settings, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(`{}`), "s.json", NativePathPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxEntries != 50 || settings.ScanMaxEntries != 2000 || settings.TimeoutMs != 30000 {
		t.Fatalf("limits = %+v", settings)
	}
	if !settings.AllowOtherSessions || !settings.AllowOtherProfiles || !settings.AllowChildren {
		t.Fatalf("switches default to allowed: %+v", settings)
	}
}

func TestTranscriptsSettingsRejectUnknownKeysNamingThem(t *testing.T) {
	for _, key := range []string{"paths", "allow_path", "approval", "nope"} {
		_, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(`{"`+key+`":1}`), "s.json", NativePathPolicy{})
		if err == nil || !strings.Contains(err.Error(), "tools.native.transcripts."+key) {
			t.Errorf("key %s: error = %v", key, err)
		}
	}
}

func TestTranscriptsSettingsCapsAndTypes(t *testing.T) {
	for _, body := range []string{`{"max_entries":0}`, `{"max_entries":501}`, `{"scan_max_entries":50001}`, `{"max_entries":100,"scan_max_entries":50}`, `{"allow_children":"no"}`, `{"timeout_ms":21600001}`} {
		if _, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(body), "s.json", NativePathPolicy{}); err == nil {
			t.Errorf("%s was accepted", body)
		}
	}
	settings, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(`{"max_entries":500,"scan_max_entries":50000,"allow_other_sessions":false,"allow_other_profiles":false,"allow_children":false}`), "s.json", NativePathPolicy{})
	if err != nil || settings.MaxEntries != 500 || settings.AllowOtherSessions || settings.AllowOtherProfiles || settings.AllowChildren {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
}
