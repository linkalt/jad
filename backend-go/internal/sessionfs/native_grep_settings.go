package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeGrepBinary             = "rg"
	defaultNativeGrepMaxResults   int64 = 200
	defaultNativeGrepMaxMatches   int64 = 20
	defaultNativeGrepMaxBytes     int64 = 64 * 1024
	defaultNativeGrepMaxLineChars int64 = 240
	defaultNativeGrepMaxFileBytes int64 = 8 * 1024 * 1024
	defaultNativeGrepTimeoutMs    int64 = 10_000
	maxNativeGrepMaxResults       int64 = 10_000
	maxNativeGrepMaxMatches       int64 = 1_000
	maxNativeGrepMaxLineChars     int64 = 4_000
	maxNativeGrepTimeoutMs        int64 = 600_000
)

// NativeGrepSettings contains the normalized limits and path policy for native.grep.
type NativeGrepSettings struct {
	Policy            NativePathPolicy
	Binary            string
	MaxResults        int64
	MaxMatchesPerFile int64
	MaxBytes          int64
	MaxLineChars      int64
	MaxFileBytes      int64
	TimeoutMs         int64
}

func normalizeNativeGrepSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath string,
	inheritedPolicy NativePathPolicy,
) (*NativeGrepSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	grepRaw, exists := nativeFields["grep"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(grepRaw, settingPath+": tools.native.grep")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "binary", "max_results", "max_matches_per_file", "max_bytes", "max_line_chars", "max_file_bytes", "timeout_ms", "paths":
		default:
			return nil, configError("%s: tools.native.grep.%s is not supported", settingPath, key)
		}
	}

	binary := defaultNativeGrepBinary
	if raw, exists := fields["binary"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &binary); err != nil || strings.TrimSpace(binary) == "" {
			return nil, configError("%s: tools.native.grep.binary must be a non-empty string", settingPath)
		}
		binary = strings.TrimSpace(binary)
	}
	settings := &NativeGrepSettings{Binary: binary}
	if settings.MaxResults, err = configuredInteger(fields, "max_results", defaultNativeGrepMaxResults, 1, maxNativeGrepMaxResults, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}
	if settings.MaxMatchesPerFile, err = configuredInteger(fields, "max_matches_per_file", defaultNativeGrepMaxMatches, 1, maxNativeGrepMaxMatches, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultNativeGrepMaxBytes, 1, maxNativeToolBytes, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", defaultNativeGrepMaxLineChars, 20, maxNativeGrepMaxLineChars, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}
	if settings.MaxFileBytes, err = configuredInteger(fields, "max_file_bytes", defaultNativeGrepMaxFileBytes, 1, maxNativeReadMaxFile, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}
	if settings.TimeoutMs, err = configuredInteger(fields, "timeout_ms", defaultNativeGrepTimeoutMs, 1, maxNativeGrepTimeoutMs, settingPath, "tools.native.grep"); err != nil {
		return nil, err
	}

	settings.Policy = cloneNativePathPolicy(inheritedPolicy)
	if raw, exists := fields["paths"]; exists {
		settings.Policy, err = normalizeNativePathPolicy(raw, globalJadRoot, settingPath+": tools.native.grep.paths")
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}
