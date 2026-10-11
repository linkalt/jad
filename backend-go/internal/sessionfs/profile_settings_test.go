package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetPathProfilePinsStoragePathsAndPreservesSettings(t *testing.T) {
	settingPath := filepath.Join(t.TempDir(), "setting.json")
	setting := `{"untouched":{"enabled":true},"paths":{"active_profile":"green","profiles":{"green":{"models":["model.json","alternate.json"],"custom":"keep-green"},"blue":{"models":["model.json","alternate.json"],"custom":"keep-blue"}}}}`
	if err := os.WriteFile(settingPath, []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	green := SessionStoragePaths{
		Messages:        "/session/messages.jsonl",
		MessagesBackup:  "/session/messages.backup.jsonl",
		CompactionCache: "/session/staging/compaction-cache.json",
		UsageCache:      "/session/staging/usage-cache.json",
		CursorCache:     "/session/staging/cursor-cache.json",
		StateContext:    "/session/staging/statecontext.json",
		Staging:         "/session/staging",
		HTTPDebug:       "/session/debug/http",
	}
	blue := green
	blue.Messages = "/session/profiles/blue/messages.jsonl"
	blue.MessagesBackup = "/session/profiles/blue/messages.backup.jsonl"
	blue.CompactionCache = "/session/profiles/blue/staging/compaction-cache.json"
	blue.UsageCache = "/session/profiles/blue/staging/usage-cache.json"
	blue.CursorCache = "/session/profiles/blue/staging/cursor-cache.json"
	blue.StateContext = "/session/profiles/blue/staging/statecontext.json"
	blue.Staging = "/session/profiles/blue/staging"
	blue.HTTPDebug = "/session/profiles/blue/debug/http"

	update, err := SetPathProfile(settingPath, "blue", "strict", map[string]SessionStoragePaths{
		"green": green,
		"blue":  blue,
	})
	if err != nil {
		t.Fatalf("SetPathProfile() error = %v", err)
	}
	if !update.Changed || update.ActiveProfile != "blue" || !reflect.DeepEqual(update.PathProfiles, []string{"green", "blue"}) {
		t.Fatalf("profile update = %+v", update)
	}
	data, err := os.ReadFile(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var untouched map[string]bool
	if err := json.Unmarshal(root["untouched"], &untouched); err != nil || !untouched["enabled"] {
		t.Fatalf("unrelated setting = %s, err=%v", root["untouched"], err)
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(root["paths"], &paths); err != nil {
		t.Fatal(err)
	}
	var active string
	if err := json.Unmarshal(paths["active_profile"], &active); err != nil || active != "blue" {
		t.Fatalf("active_profile = %q, err=%v", active, err)
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(paths["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]SessionStoragePaths{"green": green, "blue": blue} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(profiles[name], &fields); err != nil {
			t.Fatal(err)
		}
		for field, value := range expected.pathFields() {
			var got string
			if err := json.Unmarshal(fields[field], &got); err != nil || got != value {
				t.Errorf("paths.profiles.%s.%s = %q, err=%v; want %q", name, field, got, err, value)
			}
		}
		var custom string
		if err := json.Unmarshal(fields["custom"], &custom); err != nil || custom != "keep-"+name {
			t.Errorf("paths.profiles.%s.custom = %q, err=%v", name, custom, err)
		}
	}
}

func TestSetModelDefaultUpdatesOnlySelectedProfile(t *testing.T) {
	settingPath := filepath.Join(t.TempDir(), "setting.json")
	setting := `{"paths":{"active_profile":"blue","profiles":{"green":{"models":["model.json","alternate.json"],"model_default":"model.json"},"blue":{"models":["model.json"],"model_default":"model.json"}}}}`
	if err := os.WriteFile(settingPath, []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	update, err := SetModelDefault(settingPath, "green", " alternate.json ", "process")
	if err != nil {
		t.Fatalf("SetModelDefault() error = %v", err)
	}
	if !update.Changed || update.ActiveProfile != "green" || update.ModelDefault != "alternate.json" {
		t.Fatalf("model update = %+v", update)
	}
	second, err := SetModelDefault(settingPath, "green", "alternate.json", "process")
	if err != nil || second.Changed {
		t.Fatalf("idempotent model update = %+v, err=%v", second, err)
	}
	if _, err := SetModelDefault(settingPath, "green", "missing.json", "process"); err == nil {
		t.Fatal("SetModelDefault accepted a path outside the configured model list")
	}
	info, err := ReadPathProfileInfo(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.ActiveProfile != "blue" || !reflect.DeepEqual(info.PathProfiles, []string{"green", "blue"}) {
		t.Fatalf("profile info = %+v", info)
	}
	data, err := os.ReadFile(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || !containsText(string(data), `"model_default":"alternate.json"`) {
		t.Fatalf("updated settings = %s", data)
	}
}

func containsText(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

// history_cache was a storage key that nothing in Go ever wrote; it is gone, so a profile
// that still sets it fails like any other unknown key.
func TestProfileRefusesTheRemovedHistoryCacheKey(t *testing.T) {
	err := validateProfileFields("a", json.RawMessage(`{"history_cache":"x.json"}`), "setting.json: paths.profiles.a")
	if err == nil || !strings.Contains(err.Error(), "paths.profiles.a.history_cache is not a supported session path or profile model setting") {
		t.Fatalf("error = %v", err)
	}
	if err := validateProfileFields("a", json.RawMessage(`{"usage_cache":"x.json"}`), "setting.json: paths.profiles.a"); err != nil {
		t.Fatalf("a kept key must still load: %v", err)
	}
}
