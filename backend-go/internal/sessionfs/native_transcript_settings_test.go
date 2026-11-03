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
	if settings.ResponseMaxBytes != 64*1024 || settings.TimeoutMs != 30000 {
		t.Fatalf("limits = %+v", settings)
	}
	if settings.PreviewBytes != 200 || settings.ValueBytes != 80 || settings.MaxListEntries != 100 || settings.MaxDetailBytes != 16384 {
		t.Fatalf("list and detail limits = %+v", settings)
	}
	if !settings.AllowOtherSessions || !settings.AllowOtherProfiles || !settings.AllowChildren {
		t.Fatalf("other sessions, profiles and children are readable by default: %+v", settings)
	}
	if settings.AllowPath {
		t.Fatal("allow_path must be off by default")
	}
}

func TestTranscriptSettingsRejectUnknownAndOldKeysNamingThem(t *testing.T) {
	for _, key := range []string{"paths", "path", "approval", "nope", "entry_max_bytes", "max_turns_per_call"} {
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

func TestTranscriptSettingsCapTheListAndDetailLimits(t *testing.T) {
	for _, body := range []string{`{"preview_bytes":0}`, `{"preview_bytes":65537}`, `{"value_bytes":65537}`, `{"max_list_entries":5001}`, `{"max_detail_bytes":0}`, `{"max_detail_bytes":10485761}`, `{"value_bytes":null}`} {
		_, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(body), "s.json", NativePathPolicy{})
		if body == `{"value_bytes":null}` {
			if err != nil {
				t.Errorf("%s: %v", body, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s accepted", body)
		}
	}
	settings, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{"preview_bytes":65536,"value_bytes":1,"max_list_entries":5000,"max_detail_bytes":10485760}`), "s.json", NativePathPolicy{})
	if err != nil || settings.PreviewBytes != 65536 || settings.ValueBytes != 1 || settings.MaxListEntries != 5000 || settings.MaxDetailBytes != 10485760 {
		t.Fatalf("settings = %+v, err = %v", settings, err)
	}
}

func TestTranscriptRemoteSettings(t *testing.T) {
	good, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{"allow_remote":true,"remotes":["peer"]}`), "s.json", NativePathPolicy{})
	if err != nil || !good.AllowRemote || len(good.Remotes) != 1 {
		t.Fatalf("%+v %v", good, err)
	}
	if def, _ := normalizeNativeTranscriptSettings(transcriptSettingsFor(`{}`), "s.json", NativePathPolicy{}); def.AllowRemote || len(def.Remotes) != 0 {
		t.Fatal("remote reading is off by default")
	}
	for _, body := range []string{`{"allow_remote":true}`, `{"remotes":["peer"]}`, `{"allow_remote":true,"remotes":["bad name"]}`} {
		if _, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(body), "s.json", NativePathPolicy{}); err == nil {
			t.Errorf("%s must be refused", body)
		}
	}
	many, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(`{"allow_remote":true,"remotes":["peer"]}`), "s.json", NativePathPolicy{})
	if err != nil || !many.AllowRemote {
		t.Fatalf("%+v %v", many, err)
	}
	if _, err := normalizeNativeTranscriptsSettings(transcriptsSettingsFor(`{"allow_remote":true}`), "s.json", NativePathPolicy{}); err == nil {
		t.Error("allow_remote without remotes must be refused")
	}
	one, all, err := RemoteAPITranscriptSettings()
	if err != nil || !one.HidePaths || !all.HidePaths || one.AllowPath {
		t.Fatalf("the remote API reads without paths and without path: %+v %+v %v", one, all, err)
	}
}

// TestTranscriptSettingsIndexCacheEntries: 32 turn tables by default, 0 turns the cache off,
// and the count is capped.
func TestTranscriptSettingsIndexCacheEntries(t *testing.T) {
	for body, want := range map[string]int64{`{}`: 32, `{"index_cache_entries":0}`: 0, `{"index_cache_entries":4096}`: 4096} {
		settings, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(body), "s.json", NativePathPolicy{})
		if err != nil || settings.IndexCacheEntries != want {
			t.Errorf("%s: %+v, %v; want %d", body, settings, err, want)
		}
	}
	for _, body := range []string{`{"index_cache_entries":4097}`, `{"index_cache_entries":-1}`} {
		if _, err := normalizeNativeTranscriptSettings(transcriptSettingsFor(body), "s.json", NativePathPolicy{}); err == nil || !strings.Contains(err.Error(), "index_cache_entries") {
			t.Errorf("%s: %v", body, err)
		}
	}
}
