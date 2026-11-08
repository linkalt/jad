package sessionfs

import (
	"encoding/json"
)

// NativeLoadToolSettings marks tools.native.load_tool as enabled. The tool has no keys of its
// own: it lists, describes and runs the profile's tools.lazy tools.
type NativeLoadToolSettings struct{}

// normalizeNativeLoadToolSettings reads tools.native.load_tool. The value must be an empty
// object: the tool takes no settings.
func normalizeNativeLoadToolSettings(setting map[string]json.RawMessage, settingPath string) (*NativeLoadToolSettings, error) {
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
	raw, exists := nativeFields["load_tool"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.load_tool"
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	if len(keys) > 0 {
		return nil, configError("%s: %s takes no keys, but it has %q", settingPath, section, keys[0])
	}
	return &NativeLoadToolSettings{}, nil
}
