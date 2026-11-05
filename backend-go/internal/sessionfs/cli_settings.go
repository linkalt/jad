package sessionfs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Defaults and bounds of the root setting's cli section.
const (
	defaultPluginInstallTimeoutMS int64 = 5 * 60 * 1000
	defaultMCPLoginTimeoutMS      int64 = 10 * 60 * 1000
	maxCLITimeoutMS               int64 = 24 * 60 * 60 * 1000
)

var cliKeys = []string{"plugin_install_timeout_ms", "mcp_login_timeout_ms"}

// CLISettings is the root setting's cli section: defaults of jad subcommands that a flag
// can still override for one call.
type CLISettings struct {
	// PluginInstallTimeout bounds the git clone of "jad plugin install <git URL>"
	// (--timeout overrides it); zero means no limit.
	PluginInstallTimeout time.Duration
	// MCPLoginTimeout is how long "jad mcp login" waits for the sign-in (--timeout
	// overrides it).
	MCPLoginTimeout time.Duration
}

// DefaultCLISettings is the cli section when the root setting has none.
func DefaultCLISettings() CLISettings {
	return CLISettings{
		PluginInstallTimeout: time.Duration(defaultPluginInstallTimeoutMS) * time.Millisecond,
		MCPLoginTimeout:      time.Duration(defaultMCPLoginTimeoutMS) * time.Millisecond,
	}
}

func normalizeCLISettings(raw json.RawMessage, path string) (CLISettings, error) {
	settings := DefaultCLISettings()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return CLISettings{}, fmt.Errorf("%s: cli must be an object", path)
	}
	for key := range fields {
		if !containsString(cliKeys, key) {
			return CLISettings{}, fmt.Errorf("%s: cli.%s is not a setting (%s)", path, key, strings.Join(cliKeys, ", "))
		}
	}
	install, err := configuredInteger(fields, "plugin_install_timeout_ms", defaultPluginInstallTimeoutMS, 0, maxCLITimeoutMS, path, "cli")
	if err != nil {
		return CLISettings{}, err
	}
	login, err := configuredInteger(fields, "mcp_login_timeout_ms", defaultMCPLoginTimeoutMS, 1, maxCLITimeoutMS, path, "cli")
	if err != nil {
		return CLISettings{}, err
	}
	settings.PluginInstallTimeout = time.Duration(install) * time.Millisecond
	settings.MCPLoginTimeout = time.Duration(login) * time.Millisecond
	return settings, nil
}
