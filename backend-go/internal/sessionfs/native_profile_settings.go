package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeProfileMaxEntries     int64 = 50
	maxNativeProfileMaxEntries         int64 = 500
	defaultNativeProfileScanMaxEntries int64 = 2000
	maxNativeProfileScanMaxEntries     int64 = 50_000
)

var nativeProfileKeys = []string{"max_entries", "scan_max_entries", "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_remote", "remotes"}

// NativeProfileSettings holds the settings of native.profile. The listing limits and the
// switches mean what they mean for native.transcripts, with the same defaults, but are
// their own keys: profile does not read the transcripts section. A nil pointer in
// SessionConfig means the tool is not configured.
type NativeProfileSettings struct {
	// Policy is the read path rules (tools.native.read.paths): a listed transcript they deny is
	// left out, as transcripts does.
	Policy             NativePathPolicy
	MaxEntries         int64
	ScanMaxEntries     int64
	AllowOtherSessions bool
	AllowOtherProfiles bool
	// AllowChildren lets a sub-agent use the tool's list and info. Compact and reset are
	// refused to a sub-agent whatever this is.
	AllowChildren bool
	// AllowRemote and Remotes: see NativeTranscriptsSettings. The info of a remote profile is
	// read through the remote API; compact and reset are never sent.
	AllowRemote bool
	Remotes     []string
	// HidePaths leaves file paths out of every line; the remote API sets it.
	HidePaths bool
}

// normalizeNativeProfileSettings reads tools.native.profile. It is nil when the key is absent.
func normalizeNativeProfileSettings(setting map[string]json.RawMessage, settingPath string, readPolicy NativePathPolicy) (*NativeProfileSettings, error) {
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
	raw, exists := nativeFields["profile"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.profile"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeProfileKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeProfileKeys, "")
		}
	}
	settings := &NativeProfileSettings{Policy: readPolicy, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
	for _, field := range []struct {
		target             *int64
		name               string
		fallback, min, max int64
	}{
		{&settings.MaxEntries, "max_entries", defaultNativeProfileMaxEntries, 1, maxNativeProfileMaxEntries},
		{&settings.ScanMaxEntries, "scan_max_entries", defaultNativeProfileScanMaxEntries, 1, maxNativeProfileScanMaxEntries},
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
		return nil, configError("%s: %s.allow_remote needs remotes: the names of the remote entries to read", settingPath, section)
	}
	if !settings.AllowRemote && len(settings.Remotes) > 0 {
		return nil, configError("%s: %s.remotes is set but allow_remote is not true", settingPath, section)
	}
	if settings.MaxEntries > settings.ScanMaxEntries {
		return nil, configError("%s: %s.max_entries must not exceed scan_max_entries", settingPath, section)
	}
	return settings, nil
}
