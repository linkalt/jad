package sessionfs

import "encoding/json"

// mergeSettings returns base overlaid with over. Objects merge key by key, recursively;
// arrays, scalars and null replace. Neither argument is modified.
func mergeSettings(base, over map[string]json.RawMessage) map[string]json.RawMessage {
	merged := make(map[string]json.RawMessage, len(base)+len(over))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range over {
		merged[key] = mergeSettingValue(merged[key], value)
	}
	return merged
}

func mergeSettingValue(base, over json.RawMessage) json.RawMessage {
	var baseObject, overObject map[string]json.RawMessage
	if len(base) == 0 || json.Unmarshal(base, &baseObject) != nil || baseObject == nil {
		return over
	}
	if json.Unmarshal(over, &overObject) != nil || overObject == nil {
		return over
	}
	combined, err := json.Marshal(mergeSettings(baseObject, overObject))
	if err != nil {
		return over
	}
	return combined
}
