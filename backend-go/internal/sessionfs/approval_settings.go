package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	// ApprovalOff runs a call at once; ApprovalAsk waits for the user's decision first.
	ApprovalOff = "off"
	ApprovalAsk = "ask"

	// ApprovalHeadlessDeny blocks a call nobody can answer; ApprovalHeadlessAllow lets
	// unattended runs proceed.
	ApprovalHeadlessDeny  = "deny"
	ApprovalHeadlessAllow = "allow"

	// The wait values mirror TypeScript's DEFAULT_APPROVAL_* and MAX_APPROVAL_* constants,
	// which follow the request_draft values so one session behaves the same whichever
	// tool asks.
	defaultApprovalTimeoutMS    int64 = 300_000
	defaultApprovalExtendMS     int64 = 300_000
	maxApprovalTimeoutMS        int64 = 1_800_000
	minApprovalExtendMS         int64 = 1_000
	defaultApprovalPreviewBytes int64 = 4096
	minApprovalPreviewBytes     int64 = 64
	maxApprovalPreviewBytes     int64 = 65_536
)

var approvalKeys = []string{"default", "extend_ms", "headless", "max_timeout_ms", "preview_bytes", "timeout_ms"}

// ApprovalSettings are the session-wide wait and preview values shared by every
// tool whose approval is "ask".
type ApprovalSettings struct {
	// Default is the approval of every tool that can ask whose own key is absent:
	// "off" or "ask". An explicit per-tool value wins.
	Default string
	// TimeoutMS is how long an interactive run waits before the call is denied.
	TimeoutMS int64
	// ExtendMS is how much one extension adds to the deadline.
	ExtendMS int64
	// MaxTimeoutMS is the ceiling for the whole wait, extensions included.
	MaxTimeoutMS int64
	// Headless is "deny" or "allow".
	Headless string
	// PreviewBytes bounds each text field shown in the question.
	PreviewBytes int64
}

// normalizeApprovalMode reads a tool's `approval`. Only an absent key means off: a
// null or misspelt value must not quietly disable a gate.
func normalizeApprovalMode(fields map[string]json.RawMessage, fieldPath, settingPath string) (string, error) {
	raw, exists := fields["approval"]
	if !exists {
		return ApprovalOff, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil || (mode != ApprovalOff && mode != ApprovalAsk) {
		return "", configError("%s: %s.approval must be \"off\" or \"ask\"", settingPath, fieldPath)
	}
	return mode, nil
}

func normalizeApprovalSettings(raw json.RawMessage, settingPath string) (ApprovalSettings, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		decoded, err := decodeConfigObject(raw, settingPath+": approval")
		if err != nil {
			return ApprovalSettings{}, configError("%s: approval must be an object", settingPath)
		}
		fields = decoded
	}
	for key := range fields {
		if !containsString(approvalKeys, key) {
			return ApprovalSettings{}, unsupportedToolKey(settingPath+": approval", key, approvalKeys, "")
		}
	}
	bounded := func(name string, fallback, minimum, maximum int64) (int64, error) {
		// A null counts as absent here, like every other integer setting.
		value, err := configuredInteger(fields, name, fallback, minimum, maximum, settingPath, "approval")
		return value, err
	}
	var settings ApprovalSettings
	var err error
	if settings.Default, err = normalizeApprovalDefault(fields, settingPath); err != nil {
		return ApprovalSettings{}, err
	}
	if settings.TimeoutMS, err = bounded("timeout_ms", defaultApprovalTimeoutMS, 1, maxApprovalTimeoutMS); err != nil {
		return ApprovalSettings{}, err
	}
	if settings.MaxTimeoutMS, err = bounded("max_timeout_ms", maxApprovalTimeoutMS, 1, maxApprovalTimeoutMS); err != nil {
		return ApprovalSettings{}, err
	}
	if settings.TimeoutMS > settings.MaxTimeoutMS {
		return ApprovalSettings{}, configError("%s: approval.timeout_ms must not exceed max_timeout_ms", settingPath)
	}
	if settings.ExtendMS, err = bounded("extend_ms", defaultApprovalExtendMS, minApprovalExtendMS, maxApprovalTimeoutMS); err != nil {
		return ApprovalSettings{}, err
	}
	if settings.PreviewBytes, err = bounded("preview_bytes", defaultApprovalPreviewBytes, minApprovalPreviewBytes, maxApprovalPreviewBytes); err != nil {
		return ApprovalSettings{}, err
	}
	settings.Headless = ApprovalHeadlessDeny
	if rawHeadless, exists := fields["headless"]; exists && strings.TrimSpace(string(rawHeadless)) != "null" {
		var headless string
		if json.Unmarshal(rawHeadless, &headless) != nil || (headless != ApprovalHeadlessDeny && headless != ApprovalHeadlessAllow) {
			return ApprovalSettings{}, configError("%s: approval.headless must be \"deny\" or \"allow\"", settingPath)
		}
		settings.Headless = headless
	}
	return settings, nil
}
