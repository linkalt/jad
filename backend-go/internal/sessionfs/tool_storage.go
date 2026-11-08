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
	cwd, settingPath string,
) (string, error) {
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

// normalizeToolStorageSubdir resolves a native tool's storage_dir. An omitted (or null)
// value is fallback under the profile's tool storage (the cwd when toolStorageDir is
// empty). A configured value is used as given when absolute and resolves from cwd when
// relative. field names the setting in errors, e.g. tools.native.task.storage_dir.
func normalizeToolStorageSubdir(fields map[string]json.RawMessage, fallback, cwd, toolStorageDir, field, settingPath string) (string, error) {
	configured := fallback
	explicit := false
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: %s must be a non-empty string", settingPath, field)
		}
		configured = strings.TrimSpace(configured)
		explicit = true
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if !explicit && toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", field, err)
	}
	return filepath.Clean(absolute), nil
}
