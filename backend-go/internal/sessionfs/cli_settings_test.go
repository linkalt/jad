package sessionfs

import (
	"encoding/json"
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

// Load reads the cli section of global.json.
func TestLoadReadsTheCLISection(t *testing.T) {
	f := newWSFixture(t)
	w := f.workspace(t, "proj", `{}`)
	f.globalJSON(t, `{"cli":{"mcp_login_timeout_ms":2000}}`)
	loaded, err := f.load(w)
	if err != nil || loaded.CLI.MCPLoginTimeout != 2*time.Second || loaded.CLI.PluginInstallTimeout != 5*time.Minute {
		t.Fatalf("loaded cli = %+v, %v", loaded.CLI, err)
	}
	f.globalJSON(t, `{"cli":{"mcp_login_timeout_ms":0}}`)
	if _, err := f.load(w); err == nil || !strings.Contains(err.Error(), "cli.mcp_login_timeout_ms") {
		t.Fatalf("bad cli section: %v", err)
	}
	f.globalJSON(t, `{}`)
	loaded, err = f.load(w)
	if err != nil || loaded.CLI != DefaultCLISettings() {
		t.Fatalf("without a cli section: %+v, %v", loaded.CLI, err)
	}
}
