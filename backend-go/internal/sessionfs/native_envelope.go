package sessionfs

import (
	"encoding/json"
	"strings"
)

// nativeEnvelope decodes the tools object and returns the raw tools.native value. exists is
// false when tools or native is absent. A literal null native comes back as-is, so each
// caller decides what null means.
func nativeEnvelope(setting map[string]json.RawMessage, settingPath string) (json.RawMessage, bool, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, false, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, false, err
	}
	nativeRaw, exists := toolFields["native"]
	return nativeRaw, exists, nil
}

// extensionEnvelope returns the tools.extension node. A null node counts as absent.
func extensionEnvelope(setting map[string]json.RawMessage, settingPath string) (json.RawMessage, bool, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, false, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, false, err
	}
	extensionRaw, exists := toolFields["extension"]
	if !exists || strings.TrimSpace(string(extensionRaw)) == "null" {
		return nil, false, nil
	}
	return extensionRaw, true, nil
}
