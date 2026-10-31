package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeTranscriptResponseMaxBytes int64 = 64 * 1024
	defaultNativeTranscriptLineMaxBytes     int64 = 16 * 1024 * 1024
	maxNativeTranscriptLineMaxBytes         int64 = 256 * 1024 * 1024
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
	"response_max_bytes", "line_max_bytes", "scan_max_bytes",
	"suggest_max", "timeout_ms", "preview_bytes", "value_bytes", "max_list_entries", "max_detail_bytes", "allow_other_sessions", "allow_other_reasoning", "allow_other_profiles", "allow_children", "allow_path", "allow_remote", "remotes",
}

// NativeTranscriptSettings holds the limits and switches of native.transcript. The path
// policy is not a key of this tool: path reads follow the read tool's policy.
type NativeTranscriptSettings struct {
	Policy             NativePathPolicy
	ResponseMaxBytes   int64
	LineMaxBytes       int64
	ScanMaxBytes       int64
	SuggestMax         int64
	TimeoutMs          int64
	PreviewBytes       int64
	ValueBytes         int64
	MaxListEntries     int64
	MaxDetailBytes     int64
	AllowOtherSessions bool
	// AllowOtherReasoning shows the reasoning of another session in detail mode. Off by default:
	// reasoning is private planning, so a session reads another session's only when this is set.
	AllowOtherReasoning bool
	AllowOtherProfiles  bool
	AllowChildren       bool
	AllowPath           bool
	// AllowRemote lets the tool read transcripts of other JAD servers (remote:<name>/...);
	// Remotes are the remote entries it may use. Off by default.
	AllowRemote bool
	Remotes     []string
	// HidePaths leaves file paths out of every result. It is not a key: the remote API sets it.
	HidePaths bool
}

func normalizeNativeTranscriptSettings(setting map[string]json.RawMessage, settingPath string, readPolicy NativePathPolicy) (*NativeTranscriptSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists {
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
		{&settings.LineMaxBytes, "line_max_bytes", defaultNativeTranscriptLineMaxBytes, 1, maxNativeTranscriptLineMaxBytes},
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
		{&settings.AllowOtherReasoning, "allow_other_reasoning"},
		{&settings.AllowOtherProfiles, "allow_other_profiles"},
		{&settings.AllowChildren, "allow_children"},
		{&settings.AllowPath, "allow_path"},
		{&settings.AllowRemote, "allow_remote"},
	} {
		if value, has := fields[field.name]; has && strings.TrimSpace(string(value)) != "null" {
			if json.Unmarshal(value, field.target) != nil {
				return nil, configError("%s: %s.%s must be a boolean", settingPath, section, field.name)
			}
		}
	}
	if settings.Remotes, err = normalizeRemoteNames(fields["remotes"], settingPath, section); err != nil {
		return nil, err
	}
	if settings.AllowRemote && len(settings.Remotes) == 0 {
		return nil, configError("%s: %s.allow_remote needs remotes: the names of the remote entries to read", settingPath, section)
	}
	if !settings.AllowRemote && len(settings.Remotes) > 0 {
		return nil, configError("%s: %s.remotes is set but allow_remote is not true", settingPath, section)
	}
	return settings, nil
}

// RemoteAPITranscriptSettings are the settings of the transcript and transcripts tools the
// remote API runs for a token: every profile the token's globs reach is readable, no path, no
// other remote, and no file path in a result.
func RemoteAPITranscriptSettings() (NativeTranscriptSettings, NativeTranscriptsSettings, error) {
	build := func(tool string) (map[string]json.RawMessage, error) {
		raw, err := json.Marshal(map[string]any{"tools": map[string]any{"native": map[string]any{tool: map[string]any{}}}})
		if err != nil {
			return nil, err
		}
		var setting map[string]json.RawMessage
		return setting, json.Unmarshal(raw, &setting)
	}
	one, err := build("transcript")
	if err != nil {
		return NativeTranscriptSettings{}, NativeTranscriptsSettings{}, err
	}
	transcript, err := normalizeNativeTranscriptSettings(one, "remote api", NativePathPolicy{})
	if err != nil {
		return NativeTranscriptSettings{}, NativeTranscriptsSettings{}, err
	}
	many, err := build("transcripts")
	if err != nil {
		return NativeTranscriptSettings{}, NativeTranscriptsSettings{}, err
	}
	transcripts, err := normalizeNativeTranscriptsSettings(many, "remote api", NativePathPolicy{})
	if err != nil {
		return NativeTranscriptSettings{}, NativeTranscriptsSettings{}, err
	}
	transcript.AllowPath, transcript.HidePaths = false, true
	transcripts.HidePaths = true
	return *transcript, *transcripts, nil
}
