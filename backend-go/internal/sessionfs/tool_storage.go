package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func normalizeToolStorageDir(
	profileFields map[string]json.RawMessage,
	profile string,
	profileConfigured bool,
	cwd, settingPath string,
) (string, error) {
	if !profileConfigured {
		return "", nil
	}
	configured := filepath.Join(".jad", "profiles", profile, "tools")
	if raw, exists := profileFields["tool_storage"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: paths.profiles.%s.tool_storage must be a non-empty string", settingPath, profile)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		configured = filepath.Join(cwd, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve paths.profiles.%s.tool_storage: %w", profile, err)
	}
	return filepath.Clean(absolute), nil
}
