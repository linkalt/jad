package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeGlobBinary              = "rg"
	defaultNativeGlobIncludeHidden       = true
	defaultNativeGlobMaxResults    int64 = 500
	defaultNativeGlobMaxBytes      int64 = 64 * 1024
	defaultNativeGlobMaxLineChars  int64 = 1000
	defaultNativeGlobTimeoutMs     int64 = 10_000
	defaultNativeGlobKillGraceMs   int64 = 2_000
	maxNativeGlobMaxResults        int64 = 100_000
	maxNativeGlobMaxLineChars      int64 = 8_000
	maxNativeGlobTimeoutMs         int64 = 600_000
)

// NativeGlobSettings contains the normalized limits and path policy for native.glob.
type NativeGlobSettings struct {
	Policy        NativePathPolicy
	Binary        string
	IncludeHidden bool
	MaxResults    int64
	MaxBytes      int64
	MaxLineChars  int64
	TimeoutMs     int64
	// KillGraceMs is the wait between SIGTERM and SIGKILL; KillDeadlineMs and DrainAfterExitMs
	// are the bash runner's post-kill wait and post-exit pipe drain.
	KillGraceMs      int64
	KillDeadlineMs   int64
	DrainAfterExitMs int64
}

func normalizeNativeGlobSettings(
	setting map[string]json.RawMessage,
	globalJadRoot, settingPath string,
) (*NativeGlobSettings, error) {
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
	globRaw, exists := nativeFields["glob"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(globRaw, settingPath+": tools.native.glob")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "binary", "include_hidden", "max_results", "max_bytes", "max_line_chars", "timeout_ms", "kill_grace_ms", "kill_deadline_ms", "drain_after_exit_ms", "paths":
		default:
			return nil, configError("%s: tools.native.glob.%s is not supported", settingPath, key)
		}
	}

	binary := defaultNativeGlobBinary
	if raw, exists := fields["binary"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &binary); err != nil || strings.TrimSpace(binary) == "" {
			return nil, configError("%s: tools.native.glob.binary must be a non-empty string", settingPath)
		}
		binary = strings.TrimSpace(binary)
	}
	includeHidden := defaultNativeGlobIncludeHidden
	if raw, exists := fields["include_hidden"]; exists {
		if err := json.Unmarshal(raw, &includeHidden); err != nil {
			return nil, configError("%s: tools.native.glob.include_hidden must be a boolean", settingPath)
		}
	}
	settings := &NativeGlobSettings{Binary: binary, IncludeHidden: includeHidden}
	if settings.MaxResults, err = configuredInteger(fields, "max_results", defaultNativeGlobMaxResults, 1, maxNativeGlobMaxResults, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultNativeGlobMaxBytes, 1, maxNativeToolBytes, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.MaxLineChars, err = configuredInteger(fields, "max_line_chars", defaultNativeGlobMaxLineChars, 20, maxNativeGlobMaxLineChars, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.TimeoutMs, err = configuredInteger(fields, "timeout_ms", defaultNativeGlobTimeoutMs, 1, maxNativeGlobTimeoutMs, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.KillGraceMs, err = configuredInteger(fields, "kill_grace_ms", defaultNativeGlobKillGraceMs, 0, maxNativeBashKillGraceMs, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.KillDeadlineMs, err = configuredInteger(fields, "kill_deadline_ms", defaultNativeBashKillDeadlineMs, 1, maxNativeBashKillDeadlineMs, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}
	if settings.DrainAfterExitMs, err = configuredInteger(fields, "drain_after_exit_ms", defaultNativeBashDrainAfterExit, 1, maxNativeBashDrainAfterExitMs, settingPath, "tools.native.glob"); err != nil {
		return nil, err
	}

	settings.Policy = NativePathPolicy{Paths: []NativePathRule{}}
	if raw, exists := fields["paths"]; exists {
		settings.Policy, err = normalizeNativePathPolicy(raw, globalJadRoot, settingPath+": tools.native.glob.paths")
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}
