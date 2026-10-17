package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeGlobSettingsDefaultsAndIgnoresReadPolicy(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "session", "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"read":{"paths":{"allow":["allowed"],"deny":["private"]}},"glob":{}}}`),
	}
	if _, err := normalizeNativeReadSettings(setting, globalRoot, settingPath); err != nil {
		t.Fatalf("normalizeNativeReadSettings() error = %v", err)
	}
	glob, err := normalizeNativeGlobSettings(setting, globalRoot, settingPath)
	if err != nil {
		t.Fatalf("normalizeNativeGlobSettings() error = %v", err)
	}
	if glob == nil {
		t.Fatal("normalizeNativeGlobSettings() returned nil")
	}
	if glob.Binary != "rg" || !glob.IncludeHidden || glob.MaxResults != 500 || glob.MaxBytes != 64*1024 || glob.MaxLineChars != 1000 || glob.TimeoutMs != 10_000 || glob.KillGraceMs != 2000 || glob.KillDeadlineMs != 2000 || glob.DrainAfterExitMs != 500 {
		t.Fatalf("normalized glob defaults = %+v", glob)
	}
	if len(glob.Policy.Paths) != 0 || glob.Policy.UnrestrictedPaths {
		t.Fatalf("glob took the read rules: %+v", glob.Policy)
	}

	withoutRead, err := normalizeNativeGlobSettings(
		map[string]json.RawMessage{"tools": json.RawMessage(`{"native":{"glob":{}}}`)},
		globalRoot,
		settingPath,
	)
	if err != nil {
		t.Fatalf("normalizeNativeGlobSettings(without read) error = %v", err)
	}
	if len(withoutRead.Policy.Paths) != 0 || withoutRead.Policy.UnrestrictedPaths {
		t.Fatalf("default glob path policy = %+v", withoutRead.Policy)
	}
}

func TestNormalizeNativeGlobSettingsOverridesAndValidates(t *testing.T) {
	globalRoot := t.TempDir()
	settingPath := filepath.Join(globalRoot, "setting.json")
	setting := map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"glob":{"binary":" rg-custom ","include_hidden":false,"max_results":5000,"max_bytes":128,"max_line_chars":80,"timeout_ms":900,"kill_grace_ms":0,"kill_deadline_ms":700,"drain_after_exit_ms":60,"paths":{"allow":["search-root"],"deny":["blocked"]}}}}`),
	}
	glob, err := normalizeNativeGlobSettings(setting, globalRoot, settingPath)
	if err != nil {
		t.Fatalf("normalizeNativeGlobSettings(overrides) error = %v", err)
	}
	if glob.Binary != "rg-custom" || glob.IncludeHidden || glob.MaxResults != 5000 || glob.MaxBytes != 128 || glob.MaxLineChars != 80 || glob.TimeoutMs != 900 || glob.KillGraceMs != 0 || glob.KillDeadlineMs != 700 || glob.DrainAfterExitMs != 60 {
		t.Fatalf("normalized glob overrides = %+v", glob)
	}
	if len(glob.Policy.Paths) != 2 || glob.Policy.Paths[0].Path != filepath.Join(globalRoot, "search-root") || glob.Policy.Paths[1].Path != filepath.Join(globalRoot, "blocked") {
		t.Fatalf("overridden glob path policy = %+v", glob.Policy)
	}

	for _, raw := range []string{
		`{"native":{"glob":{"unknown":true}}}`,
		`{"native":{"glob":{"binary":"  "}}}`,
		`{"native":{"glob":{"include_hidden":"yes"}}}`,
		`{"native":{"glob":{"max_results":0}}}`,
		`{"native":{"glob":{"max_bytes":0}}}`,
		`{"native":{"glob":{"max_line_chars":19}}}`,
		`{"native":{"glob":{"timeout_ms":0}}}`,
		`{"native":{"glob":{"kill_grace_ms":60001}}}`,
		`{"native":{"glob":{"kill_deadline_ms":0}}}`,
		`{"native":{"glob":{"drain_after_exit_ms":0}}}`,
		`{"native":{"glob":{"drain_after_exit_ms":60001}}}`,
		`{"native":{"glob":{"paths":null}}}`,
	} {
		_, err := normalizeNativeGlobSettings(
			map[string]json.RawMessage{"tools": json.RawMessage(raw)},
			globalRoot,
			settingPath,
		)
		if err == nil || !strings.Contains(err.Error(), "tools.native.glob") {
			t.Errorf("normalizeNativeGlobSettings(%s) error = %v, want a glob config error", raw, err)
		}
	}
}
