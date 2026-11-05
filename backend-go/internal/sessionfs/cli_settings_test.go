package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLISettingsDefaultsValuesAndRefusals(t *testing.T) {
	got, err := normalizeCLISettings(nil, "s")
	if err != nil || got.PluginInstallTimeout != 5*time.Minute || got.MCPLoginTimeout != 10*time.Minute || got != DefaultCLISettings() {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	got, err = normalizeCLISettings(json.RawMessage(`{"plugin_install_timeout_ms":0,"mcp_login_timeout_ms":1500}`), "s")
	if err != nil || got.PluginInstallTimeout != 0 || got.MCPLoginTimeout != 1500*time.Millisecond {
		t.Fatalf("values = %+v, %v", got, err)
	}
	for raw, want := range map[string]string{
		`[]`:                                 "cli must be an object",
		`{"timeout_ms":1}`:                   "cli.timeout_ms is not a setting",
		`{"plugin_install_timeout_ms":-1}`:   "cli.plugin_install_timeout_ms must be a safe integer between 0 and 86400000",
		`{"mcp_login_timeout_ms":0}`:         "cli.mcp_login_timeout_ms must be a safe integer between 1 and 86400000",
		`{"mcp_login_timeout_ms":86400001}`:  "cli.mcp_login_timeout_ms must be a safe integer between 1 and 86400000",
		`{"plugin_install_timeout_ms":"5m"}`: "cli.plugin_install_timeout_ms must be a safe integer",
	} {
		if _, err := normalizeCLISettings(json.RawMessage(raw), "s"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", raw, err, want)
		}
	}
	schema := compileSchema(t, "root-setting.schema.json#/properties/cli")
	for raw, accept := range map[string]bool{
		`{}`: true, `{"plugin_install_timeout_ms":0,"mcp_login_timeout_ms":1}`: true, `{"mcp_login_timeout_ms":86400000}`: true,
		`{"timeout_ms":1}`: false, `{"plugin_install_timeout_ms":-1}`: false, `{"mcp_login_timeout_ms":0}`: false, `{"mcp_login_timeout_ms":86400001}`: false,
	} {
		if got := schemaAccepts(t, schema, raw); got != accept {
			t.Errorf("schema accepts %s = %v, want %v", raw, got, accept)
		}
		if _, err := normalizeCLISettings(json.RawMessage(raw), "s"); (err == nil) != accept {
			t.Errorf("loader accepts %s = %v (%v), want %v", raw, err == nil, err, accept)
		}
	}
}

// Load reads the cli section, and gives the defaults without a root setting.
func TestLoadReadsTheCLISection(t *testing.T) {
	dir := t.TempDir()
	setting := filepath.Join(dir, "setting.json")
	if err := os.WriteFile(setting, []byte(`{"session_roots":["sessions"],"cli":{"mcp_login_timeout_ms":2000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(LoadOptions{WorkingDir: dir, RootSettingPath: setting, Environment: map[string]string{}})
	if err != nil || loaded.CLI.MCPLoginTimeout != 2*time.Second || loaded.CLI.PluginInstallTimeout != 5*time.Minute || len(loaded.Warnings) != 0 {
		t.Fatalf("loaded cli = %+v, warnings %v, %v", loaded.CLI, loaded.Warnings, err)
	}
	if err := os.WriteFile(setting, []byte(`{"session_roots":["sessions"],"cli":{"mcp_login_timeout_ms":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(LoadOptions{WorkingDir: dir, RootSettingPath: setting, Environment: map[string]string{}}); err == nil || !strings.Contains(err.Error(), "cli.mcp_login_timeout_ms") {
		t.Fatalf("bad cli section: %v", err)
	}
	loaded, err = Load(LoadOptions{WorkingDir: dir, SessionsDir: "sessions", Environment: map[string]string{}})
	if err != nil || loaded.CLI != DefaultCLISettings() {
		t.Fatalf("without a root setting: %+v, %v", loaded.CLI, err)
	}
}
