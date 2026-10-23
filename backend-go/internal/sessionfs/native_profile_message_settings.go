package sessionfs

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

const (
	defaultNativeProfileMessageTimeoutMS      int64 = 600_000
	maxNativeProfileMessageTimeoutMS          int64 = 3_600_000
	defaultNativeProfileMessageResultMaxBytes int64 = 16 * 1024
	maxNativeProfileMessageMaxChain           int64 = 8
)

// DefaultProfileMessageMaxChain is the longest chain of profiles a message may travel
// through when max_chain is not set: the caller and one profile it reaches.
const DefaultProfileMessageMaxChain = 2

const (
	// ProfileMessageCompactOff leaves the target's transcript as it is; On compacts it
	// before the first message to that target in the caller's run.
	ProfileMessageCompactOff = "off"
	ProfileMessageCompactOn  = "on"
)

var (
	nativeProfileMessageKeys = []string{"deny_profiles", "timeout_ms", "busy_wait_ms", "result_max_bytes", "approval", "compact", "max_chain"}
	// Same shape as the HTTP layer's profile name pattern; existence is not checked at load.
	profileMessageIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// NativeProfileMessageSettings holds the settings of native.profile_message. A nil
// pointer in SessionConfig means the tool is not configured.
type NativeProfileMessageSettings struct {
	// DenyProfiles are profile ids the model may not message, in configured order. The
	// targets are the caller's session profiles minus these and the caller itself.
	DenyProfiles []string
	// TimeoutMs bounds the whole call, the wait for a busy target included.
	TimeoutMs int
	// BusyWaitMs is how long a call queues behind a run that holds the target; 0 (default)
	// fails at once with profile_busy.
	BusyWaitMs int
	// ResultMaxBytes is the most reply text one call returns.
	ResultMaxBytes int
	// MaxChain is the most profiles one message chain may hold, the first caller included.
	// 2 lets a profile message another but that one message no further.
	MaxChain int
	// Approval is "off" (default) or "ask"; ask holds every call until the user approves it.
	Approval string
	// Compact is "off" (default) or "on"; on compacts the target's transcript before the
	// first message to it in each run of the caller.
	Compact string
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
	denyProfiles, err := normalizeProfileMessageDenyProfiles(fields["deny_profiles"], settingPath, section)
	if err != nil {
		return nil, err
	}
	timeoutMS, err := configuredInteger(fields, "timeout_ms", defaultNativeProfileMessageTimeoutMS, 1, maxNativeProfileMessageTimeoutMS, settingPath, section)
	if err != nil {
		return nil, err
	}
	busyWaitMS, err := configuredInteger(fields, "busy_wait_ms", 0, 0, maxNativeProfileMessageTimeoutMS, settingPath, section)
	if err != nil {
		return nil, err
	}
	if busyWaitMS > timeoutMS {
		return nil, configError("%s: %s.busy_wait_ms must not exceed timeout_ms", settingPath, section)
	}
	resultMaxBytes, err := configuredInteger(fields, "result_max_bytes", defaultNativeProfileMessageResultMaxBytes, 1, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return nil, err
	}
	maxChain, err := configuredInteger(fields, "max_chain", int64(DefaultProfileMessageMaxChain), 2, maxNativeProfileMessageMaxChain, settingPath, section)
	if err != nil {
		return nil, err
	}
	approval, err := normalizeApprovalMode(fields, section, settingPath)
	if err != nil {
		return nil, err
	}
	compact := ProfileMessageCompactOff
	if raw, exists := fields["compact"]; exists {
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &compact) != nil || (compact != ProfileMessageCompactOff && compact != ProfileMessageCompactOn) {
			return nil, configError("%s: %s.compact must be \"off\" or \"on\"", settingPath, section)
		}
	}
	return &NativeProfileMessageSettings{DenyProfiles: denyProfiles, TimeoutMs: int(timeoutMS), BusyWaitMs: int(busyWaitMS), ResultMaxBytes: int(resultMaxBytes), MaxChain: int(maxChain), Approval: approval, Compact: compact}, nil
}

// ErrProfileMessageNoTargets is the reason a profile_message tool cannot be built.
var ErrProfileMessageNoTargets = errors.New("profile_message has no profile to message: the session has no other profile, or every one is denied")

// ProfileMessageTargets returns the session's profiles the caller may message: every
// profile except the caller's own and the denied ones, in session order.
func ProfileMessageTargets(sessionProfiles []string, caller string, denied []string) []string {
	var targets []string
	for _, id := range sessionProfiles {
		if id != caller && !slices.Contains(denied, id) {
			targets = append(targets, id)
		}
	}
	return targets
}

// ProfileMessageNoTargetsError is the config error for a profile whose profile_message
// has no target; the tool catalog and `jad check` both report it.
func ProfileMessageNoTargetsError(settingPath, profile string) error {
	return configError("%s (profile %s): %v", settingPath, profile, ErrProfileMessageNoTargets)
}

func normalizeProfileMessageDenyProfiles(raw json.RawMessage, settingPath, section string) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var profiles []string
	if json.Unmarshal(raw, &profiles) != nil {
		return nil, configError("%s: %s.deny_profiles must be an array of profile ids", settingPath, section)
	}
	seen := map[string]struct{}{}
	for _, id := range profiles {
		if !profileMessageIDPattern.MatchString(id) {
			return nil, configError("%s: %s.deny_profiles entry %q is not a valid profile id", settingPath, section, id)
		}
		if _, dup := seen[id]; dup {
			return nil, configError("%s: %s.deny_profiles lists %q more than once", settingPath, section, id)
		}
		seen[id] = struct{}{}
	}
	return profiles, nil
}
