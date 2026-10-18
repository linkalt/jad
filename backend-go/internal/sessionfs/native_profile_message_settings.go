package sessionfs

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	defaultNativeProfileMessageTimeoutMS      int64 = 600_000
	maxNativeProfileMessageTimeoutMS          int64 = 3_600_000
	defaultNativeProfileMessageResultMaxBytes int64 = 16 * 1024
)

var (
	nativeProfileMessageKeys = []string{"profiles", "timeout_ms", "result_max_bytes", "approval"}
	// Same shape as the HTTP layer's profile name pattern; existence is not checked at load.
	profileMessageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$|^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// NativeProfileMessageSettings holds the settings of native.profile_message. A nil
// pointer in SessionConfig means the tool is not configured.
type NativeProfileMessageSettings struct {
	// Profiles are the profile ids the model may message, in configured order.
	Profiles []string
	// TimeoutMs bounds the whole call.
	TimeoutMs int
	// ResultMaxBytes is the most reply text one call returns.
	ResultMaxBytes int
	// Approval is "off" (default) or "ask"; ask holds every call until the user approves it.
	Approval string
}

func normalizeNativeProfileMessageSettings(setting map[string]json.RawMessage, settingPath string) (*NativeProfileMessageSettings, error) {
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
	raw, exists := nativeFields["profile_message"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.profile_message"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeProfileMessageKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeProfileMessageKeys, "")
		}
	}
	profiles, err := normalizeProfileMessageProfiles(fields["profiles"], settingPath, section)
	if err != nil {
		return nil, err
	}
	timeoutMS, err := configuredInteger(fields, "timeout_ms", defaultNativeProfileMessageTimeoutMS, 1, maxNativeProfileMessageTimeoutMS, settingPath, section)
	if err != nil {
		return nil, err
	}
	resultMaxBytes, err := configuredInteger(fields, "result_max_bytes", defaultNativeProfileMessageResultMaxBytes, 1, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return nil, err
	}
	approval, err := normalizeApprovalMode(fields, section, settingPath)
	if err != nil {
		return nil, err
	}
	return &NativeProfileMessageSettings{Profiles: profiles, TimeoutMs: int(timeoutMS), ResultMaxBytes: int(resultMaxBytes), Approval: approval}, nil
}

func normalizeProfileMessageProfiles(raw json.RawMessage, settingPath, section string) ([]string, error) {
	var profiles []string
	if len(raw) == 0 || json.Unmarshal(raw, &profiles) != nil || len(profiles) == 0 {
		return nil, configError("%s: %s.profiles must be a non-empty array of profile ids", settingPath, section)
	}
	seen := map[string]struct{}{}
	for _, id := range profiles {
		if !profileMessageIDPattern.MatchString(id) {
			return nil, configError("%s: %s.profiles entry %q is not a valid profile id", settingPath, section, id)
		}
		if _, dup := seen[id]; dup {
			return nil, configError("%s: %s.profiles lists %q more than once", settingPath, section, id)
		}
		seen[id] = struct{}{}
	}
	return profiles, nil
}
