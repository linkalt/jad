package sessionfs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// loadConfiguredEnvFile resolves an optional `env_file` setting against base and loads it.
// base is the folder the setting's file lives in (G for global files, W/.jad for the workspace's;
// see session.go loadSessionSettingsFrom). It returns an empty path and no values when
// the field is absent. Bash, exec, process and hooks share this behaviour; section names the
// setting in errors (tools.native.bash, hooks, ...), and the wording matches the TypeScript
// loader (`<setting>: tools.native.<tool>: env_file must be a non-empty string`).
func loadConfiguredEnvFile(fields map[string]json.RawMessage, base, settingPath, section string) (string, map[string]string, error) {
	raw, exists := fields["env_file"]
	if !exists {
		return "", nil, nil
	}
	prefix := fmt.Sprintf("%s: %s", settingPath, section)
	var configured string
	if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
		return "", nil, configError("%s: env_file must be a non-empty string", prefix)
	}
	envFile := resolveFrom(base, strings.TrimSpace(configured))
	values, err := loadNativeBashEnvFile(envFile, fmt.Sprintf("%s.env_file", prefix))
	if err != nil {
		return "", nil, err
	}
	return envFile, values, nil
}
