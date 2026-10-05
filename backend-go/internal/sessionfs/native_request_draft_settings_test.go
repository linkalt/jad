package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeRequestDraftSettingsDefaultsAndOverrides(t *testing.T) {
	defaults, err := normalizeNativeRequestDraftSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"request_draft":{}}}`),
	}, "/session/setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if defaults == nil || defaults.TimeoutMS != 300_000 || defaults.HeadlessAutoAcceptTimeoutMS != 600_000 || defaults.MaxTimeoutMS != 1_800_000 || defaults.ExtendMS != 300_000 || defaults.MaxDraftBytes != 32*1024 || defaults.MaxDraftDepth != 8 || defaults.MaxTitleChars != 200 {
		t.Fatalf("request_draft defaults = %+v", defaults)
	}

	settings, err := normalizeNativeRequestDraftSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"request_draft":{"timeout_ms":1000,"headless_auto_accept_timeout_ms":2000,"max_timeout_ms":3000,"extend_ms":1000,"max_draft_bytes":2048,"max_draft_depth":4,"max_title_chars":16}}}`),
	}, "/session/setting.json")
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil || settings.TimeoutMS != 1000 || settings.HeadlessAutoAcceptTimeoutMS != 2000 || settings.MaxTimeoutMS != 3000 || settings.ExtendMS != 1000 || settings.MaxDraftBytes != 2048 || settings.MaxDraftDepth != 4 || settings.MaxTitleChars != 16 {
		t.Fatalf("normalized request_draft settings = %+v", settings)
	}
}

func TestNormalizeNativeRequestDraftSettingsRejectsInvalidSettings(t *testing.T) {
	for _, raw := range []string{
		`{"unknown":true}`,
		`{"timeout_ms":1800001}`,
		`{"timeout_ms":3000,"max_timeout_ms":2000}`,
		`{"extend_ms":999}`,
		`{"max_draft_bytes":1023}`,
		`{"max_draft_depth":33}`,
		`{"max_title_chars":7}`,
		`{"timeout_ms":1.5}`,
	} {
		_, err := normalizeNativeRequestDraftSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"request_draft":` + raw + `}}`),
		}, "/session/setting.json")
		if err == nil || !strings.Contains(err.Error(), "tools.native.request_draft") {
			t.Errorf("invalid request_draft settings %s error = %v", raw, err)
		}
	}
}

func TestLoadSessionConfigNormalizesNativeRequestDraftSettings(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"cwd":            rootDir,
		"tools": map[string]any{"native": map[string]any{"request_draft": map[string]any{
			"timeout_ms": 1_000, "headless_auto_accept_timeout_ms": 2_000, "max_timeout_ms": 3_000,
			"extend_ms": 1_000, "max_draft_bytes": 2_048, "max_draft_depth": 4, "max_title_chars": 16,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), setting, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"model":"gpt-test","api_key_env":"TEST_KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{SessionID: "session-1", Directory: sessionDir, Root: Root{GlobalJadRoot: rootDir}}
	config, err := LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	settings := config.NativeRequestDraft
	if settings == nil || settings.TimeoutMS != 1_000 || settings.HeadlessAutoAcceptTimeoutMS != 2_000 || settings.MaxTimeoutMS != 3_000 || settings.ExtendMS != 1_000 || settings.MaxDraftBytes != 2_048 || settings.MaxDraftDepth != 4 || settings.MaxTitleChars != 16 {
		t.Fatalf("LoadSessionConfig request_draft settings = %+v", settings)
	}
}
