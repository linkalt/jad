package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNativeFetchSettingsDefaultsAndOverrides(t *testing.T) {
	cwd := t.TempDir()
	defaults, err := normalizeNativeFetchSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"fetch":{}}}`),
	}, "/session/setting.json", cwd, "")
	if err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	if defaults == nil || defaults.StorageDir != filepath.Join(cwd, ".fetch") || defaults.DefaultLimit != 12_000 || defaults.MaxLimit != 50_000 || defaults.MaxResponseBytes != 2_000_000 || defaults.MaxDocumentChars != 1_000_000 || defaults.MaxStorageBytes != 64*1024*1024 || defaults.TimeoutMS != 15_000 || defaults.MaxRedirects != 5 {
		t.Fatalf("normalized fetch defaults = %+v", defaults)
	}
	if defaults.IncludeMetadata || defaults.IncludeImages || len(defaults.AllowedHosts) != 0 || len(defaults.DeniedHosts) != 0 {
		t.Fatalf("unexpected fetch defaults = %+v", defaults)
	}

	settings, err := normalizeNativeFetchSettings(map[string]json.RawMessage{
		"tools": json.RawMessage(`{"native":{"fetch":{"storage_dir":"cache/fetch","default_limit":5,"max_limit":20,"max_response_bytes":200,"max_document_chars":100,"max_storage_bytes":2000,"timeout_ms":50,"max_redirects":2,"include_metadata":true,"include_images":true,"allowed_hosts":["Example.COM","example.com","93.184.216.34"],"denied_hosts":["Blocked.Example"]}}}`),
	}, "/session/setting.json", cwd, filepath.Join(cwd, "profile-tools"))
	if err != nil {
		t.Fatalf("normalize overrides: %v", err)
	}
	if settings.StorageDir != filepath.Join(cwd, "profile-tools", "cache", "fetch") || settings.DefaultLimit != 5 || settings.MaxLimit != 20 || settings.MaxResponseBytes != 200 || settings.MaxDocumentChars != 100 || settings.MaxStorageBytes != 2_000 || settings.TimeoutMS != 50 || settings.MaxRedirects != 2 {
		t.Fatalf("normalized fetch settings = %+v", settings)
	}
	if !settings.IncludeMetadata || !settings.IncludeImages || strings.Join(settings.AllowedHosts, ",") != "example.com,93.184.216.34" || strings.Join(settings.DeniedHosts, ",") != "blocked.example" {
		t.Fatalf("normalized fetch policy = %+v", settings)
	}
}

func TestNormalizeNativeFetchSettingsRejectsInvalidSettings(t *testing.T) {
	cwd := t.TempDir()
	for _, raw := range []string{
		`{"unknown":true}`,
		`{"default_limit":0}`,
		`{"max_redirects":21}`,
		`{"timeout_ms":300001}`,
		`{"include_metadata":"yes"}`,
		`{"include_images":1}`,
		`{"allowed_hosts":["*.example.com"]}`,
		`{"denied_hosts":[" bad.example"]}`,
		`{"storage_dir":"  "}`,
		`{"default_limit":20,"max_limit":10}`,
		`{"max_limit":20,"max_document_chars":10}`,
		`{"max_document_chars":20,"max_storage_bytes":10}`,
	} {
		_, err := normalizeNativeFetchSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"fetch":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.fetch") {
			t.Errorf("invalid fetch settings %s error = %v", raw, err)
		}
	}
}

func TestLoadSessionConfigNormalizesNativeFetchSettings(t *testing.T) {
	rootDir := t.TempDir()
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"cwd":            rootDir,
		"paths": map[string]any{
			"active_profile": "work",
			"profiles": map[string]any{
				"work": map[string]any{"tool_storage": "custom-tools"},
			},
		},
		"tools": map[string]any{
			"native": map[string]any{
				"fetch": map[string]any{"storage_dir": "cache/fetch", "allowed_hosts": []string{"Example.COM"}},
			},
		},
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
	wantStorage := filepath.Join(rootDir, "custom-tools", "cache", "fetch")
	if config.NativeFetch == nil || config.NativeFetch.StorageDir != wantStorage || len(config.NativeFetch.AllowedHosts) != 1 || config.NativeFetch.AllowedHosts[0] != "example.com" {
		t.Fatalf("normalized native.fetch settings = %+v, want storage %q", config.NativeFetch, wantStorage)
	}
}
