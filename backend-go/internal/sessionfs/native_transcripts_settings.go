package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeTranscriptsMaxEntries     int64 = 50
	maxNativeTranscriptsMaxEntries         int64 = 500
	defaultNativeTranscriptsScanMaxEntries int64 = 2000
	maxNativeTranscriptsScanMaxEntries     int64 = 50_000
	defaultNativeTranscriptsTimeoutMs      int64 = 30_000
)

var nativeTranscriptsKeys = []string{
	"max_entries", "scan_max_entries", "timeout_ms", "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_remote", "remotes",
}

// NativeTranscriptsSettings holds the limits and switches of native.transcripts. The switches
// mean what they mean for native.transcript; the read path policy is the read tool's.
type NativeTranscriptsSettings struct {
	Policy             NativePathPolicy
	MaxEntries         int64
	ScanMaxEntries     int64
	TimeoutMs          int64
	AllowOtherSessions bool
	AllowOtherProfiles bool
	AllowChildren      bool
	// AllowRemote and Remotes: see NativeTranscriptSettings. Listing a remote reads its
	// profiles and runs through the remote API.
	AllowRemote bool
	Remotes     []string
	// HidePaths leaves file paths out of every line; the remote API sets it.
	HidePaths bool
}

func normalizeNativeTranscriptsSettings(setting map[string]json.RawMessage, settingPath string, readPolicy NativePathPolicy) (*NativeTranscriptsSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["transcripts"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.transcripts"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeTranscriptsKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeTranscriptsKeys, "")
		}
	}
	settings := &NativeTranscriptsSettings{Policy: readPolicy, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
	for _, field := range []struct {
		target             *int64
		name               string
		fallback, min, max int64
	}{
		{&settings.MaxEntries, "max_entries", defaultNativeTranscriptsMaxEntries, 1, maxNativeTranscriptsMaxEntries},
		{&settings.ScanMaxEntries, "scan_max_entries", defaultNativeTranscriptsScanMaxEntries, 1, maxNativeTranscriptsScanMaxEntries},
		{&settings.TimeoutMs, "timeout_ms", defaultNativeTranscriptsTimeoutMs, 1, maxToolTimeoutMs},
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
		return nil, configError("%s: %s.allow_remote needs remotes: the names of the remote entries to list", settingPath, section)
	}
	if !settings.AllowRemote && len(settings.Remotes) > 0 {
		return nil, configError("%s: %s.remotes is set but allow_remote is not true", settingPath, section)
	}
	if settings.MaxEntries > settings.ScanMaxEntries {
		return nil, configError("%s: %s.max_entries must not exceed scan_max_entries", settingPath, section)
	}
	return settings, nil
}
