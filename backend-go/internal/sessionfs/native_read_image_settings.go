package sessionfs

import (
	"encoding/json"
	"strings"
)

const defaultNativeReadImageMaxBytes int64 = 5 * 1024 * 1024

var nativeReadImageKeys = []string{"max_image_bytes"}

// NativeReadImageSettings holds the limit of native.read_image. The path policy is not
// a key of this tool: it is the read tool's policy, so one setting governs both.
type NativeReadImageSettings struct {
	Policy        NativePathPolicy
	MaxImageBytes int64
}

func normalizeNativeReadImageSettings(setting map[string]json.RawMessage, settingPath string, readPolicy NativePathPolicy) (*NativeReadImageSettings, error) {
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
	raw, exists := nativeFields["read_image"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.read_image"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeReadImageKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeReadImageKeys, "the path policy is tools.native.read.paths")
		}
	}
	settings := &NativeReadImageSettings{Policy: cloneNativePathPolicy(readPolicy)}
	if settings.MaxImageBytes, err = configuredInteger(fields, "max_image_bytes", defaultNativeReadImageMaxBytes, 1, maxNativeToolBytes, settingPath, section); err != nil {
		return nil, err
	}
	return settings, nil
}
