package sessionfs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// loadConfiguredEnvFile resolves an optional `env_file` setting against base and loads it.
// base is the session directory for a legacy session and the global root (the folder of
// the root setting.json) for a catalog-layout session, whose toolset many sessions share. It returns an empty path and no values when
// the field is absent. Bash and process share this behaviour, and the error
// wording matches the TypeScript loader (`<setting>: tools.native.<tool>:
// env_file must be a non-empty string`).
func loadConfiguredEnvFile(fields map[string]json.RawMessage, base, settingPath, tool string) (string, map[string]string, error) {
	raw, exists := fields["env_file"]
	if !exists {
		return "", nil, nil
	}
	prefix := fmt.Sprintf("%s: tools.native.%s", settingPath, tool)
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
