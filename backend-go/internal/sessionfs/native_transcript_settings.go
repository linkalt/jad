package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeTranscriptResponseMaxBytes int64 = 64 * 1024
	defaultNativeTranscriptEntryMaxBytes    int64 = 4000
	defaultNativeTranscriptLineMaxBytes     int64 = 16 * 1024 * 1024
	maxNativeTranscriptLineMaxBytes         int64 = 256 * 1024 * 1024
	defaultNativeTranscriptMaxTurns         int64 = 50
	maxNativeTranscriptMaxTurns             int64 = 1000
	defaultNativeTranscriptScanMaxBytes     int64 = 256 * 1024 * 1024
	maxNativeTranscriptScanMaxBytes         int64 = 4 * 1024 * 1024 * 1024
	defaultNativeTranscriptSuggestMax       int64 = 20
	maxNativeTranscriptSuggestMax           int64 = 500
	defaultNativeTranscriptTimeoutMs        int64 = 30_000
	defaultNativeTranscriptPreviewBytes     int64 = 200
	defaultNativeTranscriptValueBytes       int64 = 80
	maxNativeTranscriptPreviewBytes         int64 = 64 * 1024
	defaultNativeTranscriptMaxListEntries   int64 = 100
	maxNativeTranscriptMaxListEntries       int64 = 5000
	defaultNativeTranscriptMaxDetailBytes   int64 = 16 * 1024
)

var nativeTranscriptKeys = []string{
	"response_max_bytes", "entry_max_bytes", "line_max_bytes", "max_turns_per_call", "scan_max_bytes",
	"suggest_max", "timeout_ms", "preview_bytes", "value_bytes", "max_list_entries", "max_detail_bytes", "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_path",
}

// NativeTranscriptSettings holds the limits and switches of native.transcript. The path
// policy is not a key of this tool: path reads follow the read tool's policy.
type NativeTranscriptSettings struct {
	Policy             NativePathPolicy
	ResponseMaxBytes   int64
	EntryMaxBytes      int64
	LineMaxBytes       int64
	MaxTurnsPerCall    int64
	ScanMaxBytes       int64
	SuggestMax         int64
	TimeoutMs          int64
	PreviewBytes       int64
	ValueBytes         int64
	MaxListEntries     int64
	MaxDetailBytes     int64
	AllowOtherSessions bool
	AllowOtherProfiles bool
	AllowChildren      bool
	AllowPath          bool
}

func normalizeNativeTranscriptSettings(setting map[string]json.RawMessage, settingPath string, readPolicy NativePathPolicy) (*NativeTranscriptSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["transcript"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.transcript"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeTranscriptKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeTranscriptKeys, "")
		}
	}
	settings := &NativeTranscriptSettings{Policy: readPolicy, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
	for _, field := range []struct {
		target             *int64
		name               string
		fallback, min, max int64
	}{
		{&settings.ResponseMaxBytes, "response_max_bytes", defaultNativeTranscriptResponseMaxBytes, 1, maxNativeToolBytes},
		{&settings.EntryMaxBytes, "entry_max_bytes", defaultNativeTranscriptEntryMaxBytes, 1, maxNativeToolBytes},
		{&settings.LineMaxBytes, "line_max_bytes", defaultNativeTranscriptLineMaxBytes, 1, maxNativeTranscriptLineMaxBytes},
		{&settings.MaxTurnsPerCall, "max_turns_per_call", defaultNativeTranscriptMaxTurns, 1, maxNativeTranscriptMaxTurns},
		{&settings.ScanMaxBytes, "scan_max_bytes", defaultNativeTranscriptScanMaxBytes, 1, maxNativeTranscriptScanMaxBytes},
		{&settings.SuggestMax, "suggest_max", defaultNativeTranscriptSuggestMax, 1, maxNativeTranscriptSuggestMax},
		{&settings.PreviewBytes, "preview_bytes", defaultNativeTranscriptPreviewBytes, 1, maxNativeTranscriptPreviewBytes},
		{&settings.ValueBytes, "value_bytes", defaultNativeTranscriptValueBytes, 1, maxNativeTranscriptPreviewBytes},
		{&settings.MaxListEntries, "max_list_entries", defaultNativeTranscriptMaxListEntries, 1, maxNativeTranscriptMaxListEntries},
		{&settings.MaxDetailBytes, "max_detail_bytes", defaultNativeTranscriptMaxDetailBytes, 1, maxNativeToolBytes},
		{&settings.TimeoutMs, "timeout_ms", defaultNativeTranscriptTimeoutMs, 1, maxToolTimeoutMs},
	} {
		if *field.target, err = configuredInteger(fields, field.name, field.fallback, field.min, field.max, settingPath, section); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		target *bool
		name   string
	}{
		{&settings.AllowOtherSessions, "allow_other_sessions"},
		{&settings.AllowOtherProfiles, "allow_other_profiles"},
		{&settings.AllowChildren, "allow_children"},
		{&settings.AllowPath, "allow_path"},
	} {
		if value, has := fields[field.name]; has && strings.TrimSpace(string(value)) != "null" {
			if json.Unmarshal(value, field.target) != nil {
				return nil, configError("%s: %s.%s must be a boolean", settingPath, section, field.name)
			}
		}
	}
	return settings, nil
}
