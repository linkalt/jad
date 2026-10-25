package sessionfs

import (
	"encoding/json"
	"strings"
	"testing"
)

func transcriptSettingsFor(body string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"transcript":` + body + `}}`)}
}

func TestTranscriptSettingsAreOffUnlessConfigured(t *testing.T) {
	settings, err := normalizeNativeTranscriptSettings(map[string]json.RawMessage{}, "s.json", NativePathPolicy{})
	if err != nil || settings != nil {
		t.Fatalf("settings = %v, err = %v", settings, err)
	}
}

func TestTranscriptSettingsDefaults(t *testing.T) {
	settings, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{}`), "s.json", NativePathPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.ResponseMaxBytes != 64*1024 || settings.EntryMaxBytes != 4000 || settings.MaxTurnsPerCall != 50 || settings.TimeoutMs != 30000 {
		t.Fatalf("limits = %+v", settings)
	}
	if !settings.AllowOtherSessions || !settings.AllowOtherProfiles || !settings.AllowChildren {
		t.Fatalf("other sessions, profiles and children are readable by default: %+v", settings)
	}
	if settings.AllowPath {
		t.Fatal("allow_path must be off by default")
	}
}

func TestTranscriptSettingsRejectUnknownAndOldKeysNamingThem(t *testing.T) {
	for _, key := range []string{"paths", "path", "approval", "nope"} {
		_, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{"`+key+`":1}`), "s.json", NativePathPolicy{})
		if err == nil || !strings.Contains(err.Error(), "tools.native.transcript."+key) {
			t.Errorf("key %s: error = %v", key, err)
		}
	}
}

func TestTranscriptSettingsKeepTheReadPolicy(t *testing.T) {
	policy := NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: "/x"}}}
	settings, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{}`), "s.json", policy)
	if err != nil || len(settings.Policy.Paths) != 1 {
		t.Fatalf("policy = %+v, err = %v", settings, err)
	}
}
