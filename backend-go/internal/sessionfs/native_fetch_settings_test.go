package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
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

func TestNormalizeNativeFetchSettingsKeepAlive(t *testing.T) {
	cwd := t.TempDir()
	load := func(raw string) (*NativeFetchSettings, error) {
		return normalizeNativeFetchSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"fetch":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
	}
	defaults, err := load(`{}`)
	if err != nil || defaults.KeepAliveMS != 30_000 || defaults.MaxKeepAliveHosts != 8 {
		t.Fatalf("keep-alive defaults = %+v, %v", defaults, err)
	}
	custom, err := load(`{"keep_alive_ms":0,"max_keep_alive_hosts":3}`)
	if err != nil || custom.KeepAliveMS != 0 || custom.MaxKeepAliveHosts != 3 {
		t.Fatalf("keep-alive overrides = %+v, %v", custom, err)
	}
	for _, raw := range []string{
		`{"keep_alive_ms":-1}`,
		`{"keep_alive_ms":300001}`,
		`{"keep_alive_ms":"30"}`,
		`{"max_keep_alive_hosts":0}`,
		`{"max_keep_alive_hosts":65}`,
	} {
		if _, err := load(raw); err == nil || !strings.Contains(err.Error(), "tools.native.fetch") {
			t.Errorf("load(%s) error = %v", raw, err)
		}
	}
}

func TestNormalizeNativeFetchSettingsUserAgentTruncateAndCache(t *testing.T) {
	cwd := t.TempDir()
	load := func(raw string) (*NativeFetchSettings, error) {
		return normalizeNativeFetchSettings(map[string]json.RawMessage{
			"tools": json.RawMessage(`{"native":{"fetch":` + raw + `}}`),
		}, "/session/setting.json", cwd, "")
	}
	for _, raw := range []string{`{}`, `{"user_agent":null,"truncate_oversized_response":null,"cache_ttl_ms":null}`} {
		defaults, err := load(raw)
		if err != nil || defaults.UserAgent != "Mozilla/5.0 (compatible; jad-fetch/1.0)" || defaults.TruncateOversizedResponse || defaults.CacheTTLMS != 0 {
			t.Fatalf("defaults for %s = %+v, %v", raw, defaults, err)
		}
	}
	custom, err := load(`{"user_agent":"  my-bot/2.0  ","truncate_oversized_response":true,"cache_ttl_ms":86400000}`)
	if err != nil || custom.UserAgent != "my-bot/2.0" || !custom.TruncateOversizedResponse || custom.CacheTTLMS != 86_400_000 {
		t.Fatalf("overrides = %+v, %v", custom, err)
	}
	if long, err := load(`{"user_agent":"` + strings.Repeat("a", 512) + `"}`); err != nil || len(long.UserAgent) != 512 {
		t.Fatalf("512-character user agent = %+v, %v", long, err)
	}
}

// The schema in sample/schemes is the user-facing contract for tools.native.fetch.
func TestFetchSchemaListsTheKeysTheLoaderAccepts(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tools struct {
				Properties struct {
					Native struct {
						Properties struct {
							Fetch struct {
								Properties map[string]struct {
									Default any `json:"default"`
								} `json:"properties"`
							} `json:"fetch"`
						} `json:"properties"`
					} `json:"native"`
				} `json:"properties"`
			} `json:"tools"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema.Properties.Tools.Properties.Native.Properties.Fetch.Properties
	var got []string
	for key := range properties {
		got = append(got, key)
	}
	want := append([]string(nil), nativeFetchKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema keys %v, loader accepts %v", got, want)
	}
	defaults := map[string]any{
		"user_agent":                  defaultNativeFetchUserAgent,
		"truncate_oversized_response": defaultNativeFetchTruncateOversized,
		"cache_ttl_ms":                float64(defaultNativeFetchCacheTTLMS),
		"include_metadata":            defaultNativeFetchIncludeMetadata,
		"include_images":              defaultNativeFetchIncludeImages,
	}
	for key, value := range defaults {
		if properties[key].Default != value {
			t.Errorf("schema default of %s = %v, loader default %v", key, properties[key].Default, value)
		}
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
		`{"user_agent":""}`,
		`{"user_agent":"   "}`,
		`{"user_agent":"bad\nagent"}`,
		`{"user_agent":"` + strings.Repeat("a", 513) + `"}`,
		`{"user_agent":7}`,
		`{"cache_ttl_ms":-1}`,
		`{"cache_ttl_ms":86400001}`,
		`{"truncate_oversized_response":"yes"}`,
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
	config, err := loadLayoutConfig(t, layouttest.Spec{CWD: rootDir, Settings: map[string]any{"tool_storage_dir": "custom-tools"}, Native: map[string]any{
		"fetch": map[string]any{"storage_dir": "cache/fetch", "allowed_hosts": []string{"Example.COM"}},
	}})
	if err != nil {
		t.Fatalf("LoadSessionConfig() error = %v", err)
	}
	wantStorage := filepath.Join(rootDir, "custom-tools", "session-1", "dev", "coder", "tools", "cache", "fetch")
	if config.NativeFetch == nil || config.NativeFetch.StorageDir != wantStorage || len(config.NativeFetch.AllowedHosts) != 1 || config.NativeFetch.AllowedHosts[0] != "example.com" {
		t.Fatalf("normalized native.fetch settings = %+v, want storage %q", config.NativeFetch, wantStorage)
	}
}
