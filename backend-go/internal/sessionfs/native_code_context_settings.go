package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultNativeCodeContextStorageDir                = ".code_context"
	defaultNativeCodeContextMaxEntries          int64 = 500
	maxNativeCodeContextMaxEntries              int64 = 100_000
	defaultNativeCodeContextMaxDescriptionChars int64 = 4_000
	maxNativeCodeContextMaxDescriptionChars     int64 = 100_000
	defaultNativeCodeContextMaxRangeLines       int64 = 2_000
	maxNativeCodeContextMaxRangeLines           int64 = 1_000_000
	defaultNativeCodeContextMaxFileBytes        int64 = 4 * 1024 * 1024
	// defaultNativeCodeContextMaxOutputBytes is about 100K tokens of code: the ceiling of
	// one get, not a target.
	defaultNativeCodeContextMaxOutputBytes  int64 = 400_000
	defaultNativeCodeContextMaxStorageBytes int64 = 1 * 1024 * 1024
)

var nativeCodeContextKeys = []string{"storage_dir", "paths", "max_entries", "max_description_chars", "max_range_lines", "max_file_bytes", "max_output_bytes", "max_storage_bytes"}

// NativeCodeContextSettings configure native.code_context: a per-task list of exact
// source ranges, each with a description, that get reads back as code.
type NativeCodeContextSettings struct {
	// StorageDir holds one JSON file per task. Relative to the profile's tool_storage
	// (or the cwd without a profile); an absolute directory shares it between profiles.
	StorageDir string
	// Policy is the tool's own read rules: which files a range may name.
	Policy NativePathPolicy
	// MaxEntries bounds the entries of one task.
	MaxEntries          int64
	MaxDescriptionChars int64
	// MaxRangeLines bounds end-start+1 of one entry.
	MaxRangeLines int64
	// MaxFileBytes is the largest file a range may be read from.
	MaxFileBytes int64
	// MaxOutputBytes bounds the code of one get; entries of the highest priority numbers
	// are left out first.
	MaxOutputBytes  int64
	MaxStorageBytes int64
}

func normalizeNativeCodeContextSettings(setting map[string]json.RawMessage, globalJadRoot, settingPath, cwd, toolStorageDir string) (*NativeCodeContextSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["code_context"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.code_context"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeCodeContextKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeCodeContextKeys, "")
		}
	}
	settings := &NativeCodeContextSettings{Policy: NativePathPolicy{Paths: []NativePathRule{}}}
	if rawPaths, exists := fields["paths"]; exists {
		if settings.Policy, err = normalizeNativePathPolicy(rawPaths, globalJadRoot, settingPath+": "+section+".paths"); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		name               string
		target             *int64
		fallback, min, max int64
	}{
		{"max_entries", &settings.MaxEntries, defaultNativeCodeContextMaxEntries, 1, maxNativeCodeContextMaxEntries},
		{"max_description_chars", &settings.MaxDescriptionChars, defaultNativeCodeContextMaxDescriptionChars, 1, maxNativeCodeContextMaxDescriptionChars},
		{"max_range_lines", &settings.MaxRangeLines, defaultNativeCodeContextMaxRangeLines, 1, maxNativeCodeContextMaxRangeLines},
		{"max_file_bytes", &settings.MaxFileBytes, defaultNativeCodeContextMaxFileBytes, 1, maxNativeToolBytes},
		{"max_output_bytes", &settings.MaxOutputBytes, defaultNativeCodeContextMaxOutputBytes, 1, maxNativeToolBytes},
		{"max_storage_bytes", &settings.MaxStorageBytes, defaultNativeCodeContextMaxStorageBytes, 1, maxNativeToolBytes},
	} {
		if *field.target, err = configuredInteger(fields, field.name, field.fallback, field.min, field.max, settingPath, section); err != nil {
			return nil, err
		}
	}
	if settings.StorageDir, err = normalizeNativeCodeContextStorageDir(fields, cwd, toolStorageDir, settingPath); err != nil {
		return nil, err
	}
	return settings, nil
}

// normalizeNativeCodeContextStorageDir follows the task tool: a relative directory is
// under the profile's tool_storage, or the cwd when no profile is selected.
func normalizeNativeCodeContextStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeCodeContextStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.code_context.storage_dir must be a non-empty string", settingPath)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve tools.native.code_context.storage_dir: %w", err)
	}
	return filepath.Clean(absolute), nil
}
