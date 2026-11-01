package sessionfs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func profileSettingsFor(body string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"profile":` + body + `}}`)}
}

func TestProfileSettingsAreOffUnlessConfigured(t *testing.T) {
	settings, err := normalizeNativeProfileSettings(map[string]json.RawMessage{}, "s.json", NativePathPolicy{})
	if err != nil || settings != nil {
		t.Fatalf("settings = %v, err = %v", settings, err)
	}
}

// The defaults and bounds are the transcripts ones; the test pins both so a change to one
// without the other shows up here.
func TestProfileSettingsDefaultsMatchTranscripts(t *testing.T) {
	settings, err := normalizeNativeProfileSettings(profileSettingsFor(`{}`), "s.json", NativePathPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxEntries != 50 || settings.ScanMaxEntries != 2000 {
		t.Fatalf("limits = %+v", settings)
	}
	if !settings.AllowOtherSessions || !settings.AllowOtherProfiles || !settings.AllowChildren {
		t.Fatalf("switches default to allowed: %+v", settings)
	}
	if defaultNativeProfileMaxEntries != defaultNativeTranscriptsMaxEntries || maxNativeProfileMaxEntries != maxNativeTranscriptsMaxEntries ||
		defaultNativeProfileScanMaxEntries != defaultNativeTranscriptsScanMaxEntries || maxNativeProfileScanMaxEntries != maxNativeTranscriptsScanMaxEntries {
		t.Fatal("profile limits differ from transcripts limits")
	}
}

func TestProfileSettingsRejectBadValuesAndUnknownKeys(t *testing.T) {
	for _, body := range []string{`{"max_entries":0}`, `{"max_entries":501}`, `{"scan_max_entries":50001}`, `{"max_entries":100,"scan_max_entries":50}`, `{"allow_children":"yes"}`, `{"timeout_ms":1000}`, `{"paths":1}`} {
		if _, err := normalizeNativeProfileSettings(profileSettingsFor(body), "s.json", NativePathPolicy{}); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
	_, err := normalizeNativeProfileSettings(profileSettingsFor(`{"nope":1}`), "s.json", NativePathPolicy{})
	if err == nil || !strings.Contains(err.Error(), "tools.native.profile.nope") {
		t.Fatalf("unknown key error = %v", err)
	}
}

// The sample schema's bounds and defaults for the profile section match the loader's.
func TestProfileSchemaBoundsMatchTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties map[string]struct {
							Properties map[string]struct {
								Minimum *int64 `json:"minimum"`
								Maximum *int64 `json:"maximum"`
								Default any    `json:"default"`
							} `json:"properties"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	native := schema.Properties.Tools.Properties.Native.Properties
	for _, check := range []struct {
		tool   string
		bounds map[string]struct{ fallback, min, max int64 }
	}{
		{"profile", map[string]struct{ fallback, min, max int64 }{
			"max_entries":      {defaultNativeProfileMaxEntries, 1, maxNativeProfileMaxEntries},
			"scan_max_entries": {defaultNativeProfileScanMaxEntries, 1, maxNativeProfileScanMaxEntries},
		}},
		{"transcripts", map[string]struct{ fallback, min, max int64 }{
			"max_entries":      {defaultNativeTranscriptsMaxEntries, 1, maxNativeTranscriptsMaxEntries},
			"scan_max_entries": {defaultNativeTranscriptsScanMaxEntries, 1, maxNativeTranscriptsScanMaxEntries},
		}},
	} {
		section := native[check.tool].Properties
		for name, want := range check.bounds {
			field, ok := section[name]
			if !ok || field.Minimum == nil || field.Maximum == nil || *field.Minimum != want.min || *field.Maximum != want.max || field.Default != float64(want.fallback) {
				t.Errorf("%s schema %s = %+v, loader wants default %d within [%d, %d]", check.tool, name, field, want.fallback, want.min, want.max)
			}
		}
	}
	section := native["profile"].Properties
	if len(section) != len(nativeProfileKeys) {
		t.Errorf("schema keys = %d, loader keys = %d", len(section), len(nativeProfileKeys))
	}
	for _, name := range nativeProfileKeys {
		if _, ok := section[name]; !ok {
			t.Errorf("schema has no %s", name)
		}
	}
}
