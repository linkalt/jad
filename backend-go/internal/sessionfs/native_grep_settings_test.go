package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeGrepSettingsDefaultsAndInheritsReadPolicy(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"read":{"paths":{"allow":["allowed"],"deny":["private"]}},"grep":{}}}`),
	}
	read, err := normalizeNativeReadSettings(setting, globalRoot, settingPath)
	if err != nil {
		t.Fatalf("normalizeNativeReadSettings() error = %v", err)
	}
	grep, err := normalizeNativeGrepSettings(setting, globalRoot, settingPath, read.Policy)
	if err != nil {
		t.Fatalf("normalizeNativeGrepSettings() error = %v", err)
	}
	if grep == nil {
		t.Fatal("normalizeNativeGrepSettings() returned nil")
	}
	if grep.Binary != "rg" || grep.MaxResults != 200 || grep.MaxMatchesPerFile != 20 || grep.MaxBytes != 64*1024 || grep.MaxLineChars != 240 || grep.MaxFileBytes != 8*1024*1024 || grep.TimeoutMs != 10_000 {
		t.Fatalf("normalized grep defaults = %+v", grep)
	}
	wantPolicy := NativePathPolicy{
		Paths: []NativePathRule{
			{Action: "allow", Path: filepath.Join(globalRoot, "allowed")},
			{Action: "deny", Path: filepath.Join(globalRoot, "private")},
		},
	}
	if !reflect.DeepEqual(grep.Policy, wantPolicy) {
		t.Fatalf("inherited grep path policy = %+v, want %+v", grep.Policy, wantPolicy)
	}

	withoutRead, err := normalizeNativeGrepSettings(
		map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"grep":{}}}`)},
		globalRoot,
		settingPath,
		NativePathPolicy{Paths: []NativePathRule{}},
	)
	if err != nil {
		t.Fatalf("normalizeNativeGrepSettings(without read) error = %v", err)
	}
	if len(withoutRead.Policy.Paths) != 0 || withoutRead.Policy.UnrestrictedPaths {
		t.Fatalf("default grep path policy = %+v", withoutRead.Policy)
	}
}

func TestNormalizeNativeGrepSettingsOverridesAndValidates(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"grep":{"binary":" rg-custom ","max_results":500,"max_matches_per_file":7,"max_bytes":128,"max_line_chars":80,"max_file_bytes":2048,"timeout_ms":900,"paths":{"allow":["search-root"],"deny":["blocked"]}}}}`),
	}
	grep, err := normalizeNativeGrepSettings(setting, globalRoot, settingPath, NativePathPolicy{Paths: []NativePathRule{{Action: "deny", Path: "/inherited"}}})
	if err != nil {
		t.Fatalf("normalizeNativeGrepSettings(overrides) error = %v", err)
	}
	if grep.Binary != "rg-custom" || grep.MaxResults != 500 || grep.MaxMatchesPerFile != 7 || grep.MaxBytes != 128 || grep.MaxLineChars != 80 || grep.MaxFileBytes != 2048 || grep.TimeoutMs != 900 {
		t.Fatalf("normalized grep overrides = %+v", grep)
	}
	if len(grep.Policy.Paths) != 2 || grep.Policy.Paths[0].Path != filepath.Join(globalRoot, "search-root") || grep.Policy.Paths[1].Path != filepath.Join(globalRoot, "blocked") {
		t.Fatalf("overridden grep path policy = %+v", grep.Policy)
	}

	for _, raw := range []string{
		`{"native":{"grep":{"unknown":true}}}`,
		`{"native":{"grep":{"binary":"  "}}}`,
		`{"native":{"grep":{"max_results":0}}}`,
		`{"native":{"grep":{"max_matches_per_file":0}}}`,
		`{"native":{"grep":{"max_bytes":0}}}`,
		`{"native":{"grep":{"max_line_chars":19}}}`,
		`{"native":{"grep":{"max_file_bytes":0}}}`,
		`{"native":{"grep":{"timeout_ms":0}}}`,
		`{"native":{"grep":{"paths":null}}}`,
	} {
		_, err := normalizeNativeGrepSettings(
			map[string]json.RawMessage{"tools": json.RawMessage(raw)},
			globalRoot,
			settingPath,
			NativePathPolicy{Paths: []NativePathRule{}},
		)
		if err == nil || !strings.Contains(err.Error(), "tools.native.grep") {
			t.Errorf("normalizeNativeGrepSettings(%s) error = %v, want a grep config error", raw, err)
		}
	}
}
