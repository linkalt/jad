package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	// defaultNativeSkillMaxBytes is large enough for the biggest skills in use
	// (about 21 KiB) with room to grow.
	defaultNativeSkillMaxBytes int64 = 128 * 1024
)

var nativeSkillKeys = []string{"max_bytes"}

// NativeSkillSettings contains the normalized limits for native.skill.
type NativeSkillSettings struct {
	// MaxBytes is the skill instruction bytes returned per call; a longer skill
	// is cut and the result says so.
	MaxBytes int64
}

func normalizeNativeSkillSettings(setting map[string]json.RawMessage, settingPath string) (*NativeSkillSettings, error) {
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
	raw, exists := nativeFields["skill"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.skill"
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeSkillKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeSkillKeys, "")
		}
	}
	maxBytes, err := configuredInteger(fields, "max_bytes", defaultNativeSkillMaxBytes, 1, maxNativeToolBytes, settingPath, section)
	if err != nil {
		return nil, err
	}
	return &NativeSkillSettings{MaxBytes: maxBytes}, nil
}
