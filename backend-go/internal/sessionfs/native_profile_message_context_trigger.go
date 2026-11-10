package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	// ProfileMessageContextOff leaves the target's transcript as it is (the default).
	ProfileMessageContextOff = "off"
	// ProfileMessageContextCompact compacts the target's transcript, with the target's own
	// compaction settings, before the first message to it in each run of the caller.
	ProfileMessageContextCompact = "compact"
	// ProfileMessageContextReset clears the target's whole history, and the data derived from
	// it, on the same trigger as compact. It is destructive and never on by default.
	ProfileMessageContextReset = "reset"
)

// profileMessageContextTriggerKeys are the keys of tools.native.profile_message.context_trigger.
var profileMessageContextTriggerKeys = []string{"action", "after"}

// ProfileMessageContextTrigger is what happens to the target's transcript before a
// profile_message run: Action off, compact or reset, once the transcript passes After.
type ProfileMessageContextTrigger struct {
	// Action is "off" (default), "compact" or "reset".
	Action string
	// After is the input tokens above which Action runs: the context of the target, read from
	// its messages.jsonl, must have more than this many. Zero (default) acts on any target
	// whose messages.jsonl has content, without counting tokens. It is carried when Action is off.
	After int64
}

// normalizeProfileMessageContextTrigger reads context_trigger {action, after}, or the legacy
// compact and compact_after keys it replaces, which map onto it (compact on is action
// compact, compact_after is after). Writing both shapes is refused rather than one
// silently winning. An explicit null is read as absent, like the other numeric settings.
func normalizeProfileMessageContextTrigger(fields map[string]json.RawMessage, settingPath, section string) (ProfileMessageContextTrigger, error) {
	legacy := ProfileMessageContextTrigger{Action: ProfileMessageContextOff}
	legacyCompact, hasCompact := fields["compact"]
	if hasCompact {
		compact := ProfileMessageContextOff
		if strings.TrimSpace(string(legacyCompact)) == "null" || json.Unmarshal(legacyCompact, &compact) != nil || (compact != "off" && compact != "on") {
			return legacy, configError("%s: %s.compact must be \"off\" or \"on\"", settingPath, section)
		}
		if compact == "on" {
			legacy.Action = ProfileMessageContextCompact
		}
	}
	var err error
	if legacy.After, err = configuredInteger(fields, "compact_after", 0, 0, maxSafeInteger, settingPath, section); err != nil {
		return legacy, err
	}
	hasLegacy := hasCompact || nonNullField(fields, "compact_after")
	raw, exists := fields["context_trigger"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return legacy, nil
	}
	if hasLegacy {
		return legacy, configError("%s: %s.context_trigger cannot be combined with the legacy compact or compact_after; use only context_trigger {action, after}", settingPath, section)
	}
	triggerSection := section + ".context_trigger"
	object, err := decodeConfigObject(raw, settingPath+": "+triggerSection)
	if err != nil {
		return legacy, err
	}
	if err := rejectUnknownKeys(object, settingPath+": "+triggerSection, profileMessageContextTriggerKeys); err != nil {
		return legacy, err
	}
	trigger := ProfileMessageContextTrigger{Action: ProfileMessageContextOff}
	if action, ok := object["action"]; ok && strings.TrimSpace(string(action)) != "null" {
		var text string
		if json.Unmarshal(action, &text) != nil || (text != ProfileMessageContextOff && text != ProfileMessageContextCompact && text != ProfileMessageContextReset) {
			return legacy, configError("%s: %s.action must be \"off\", \"compact\" or \"reset\"", settingPath, triggerSection)
		}
		trigger.Action = text
	}
	if trigger.After, err = configuredInteger(object, "after", 0, 0, maxSafeInteger, settingPath, triggerSection); err != nil {
		return legacy, err
	}
	return trigger, nil
}

func nonNullField(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	return exists && strings.TrimSpace(string(raw)) != "null"
}
