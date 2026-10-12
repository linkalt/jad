package sessionfs

import (
	"encoding/json"
	"strings"
)

// The ways a task, process or subagent statecontext section can render.
const (
	NativeStateContextModeItems   = "items"
	NativeStateContextModeSummary = "summary"
)

// normalizeNativeStateContextMode reads statecontext.mode; absent or null means items.
func normalizeNativeStateContextMode(fields map[string]json.RawMessage, where string) (string, error) {
	raw, exists := fields["mode"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return NativeStateContextModeItems, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil || (mode != NativeStateContextModeItems && mode != NativeStateContextModeSummary) {
		return "", configError("%s.mode must be %q or %q", where, NativeStateContextModeItems, NativeStateContextModeSummary)
	}
	return mode, nil
}

// normalizeNativeStateContextDetail reads the states a summary still lists in full. It
// is a summary-only key and must name states the section covers; when absent, the
// defaults the section covers are used.
func normalizeNativeStateContextDetail(fields map[string]json.RawMessage, key, where, mode string, covered, defaults []string) ([]string, error) {
	raw, exists := fields[key]
	if exists && strings.TrimSpace(string(raw)) == "null" {
		exists = false
	}
	if mode != NativeStateContextModeSummary {
		if exists {
			return nil, configError("%s.%s is only valid with mode %q", where, key, NativeStateContextModeSummary)
		}
		return nil, nil
	}
	if !exists {
		detail := make([]string, 0, len(defaults))
		for _, name := range defaults {
			if containsString(covered, name) {
				detail = append(detail, name)
			}
		}
		return detail, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || names == nil {
		return nil, configError("%s.%s must be an array of states", where, key)
	}
	detail := make([]string, 0, len(names))
	for _, name := range names {
		if !containsString(covered, name) {
			return nil, configError("%s.%s: %q is not one of the states this section covers", where, key, name)
		}
		if !containsString(detail, name) {
			detail = append(detail, name)
		}
	}
	return detail, nil
}
