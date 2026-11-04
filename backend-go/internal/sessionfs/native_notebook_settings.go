package sessionfs

import (
	"encoding/json"
)

const (
	defaultNativeNotebookMaxOutputChars int64 = 2000
	maxNativeNotebookMaxOutputChars     int64 = 1_000_000
)

var nativeNotebookKeys = []string{"max_output_chars"}

// NativeNotebookSettings configure native.notebook, which reads and changes the cells of a
// Jupyter notebook. Where it may write, how large a file may be and whether a backup is
// made are the edit tool's settings: a profile that may edit a file may edit the notebook,
// and no other. MaxOutputChars cuts each text output a read shows.
type NativeNotebookSettings struct {
	Edit           NativeEditSettings
	MaxOutputChars int64
}

func normalizeNativeNotebookSettings(setting map[string]json.RawMessage, edit *NativeEditSettings, settingPath string) (*NativeNotebookSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["notebook"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.notebook"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if !containsString(nativeNotebookKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeNotebookKeys, "")
		}
	}
	if edit == nil {
		return nil, configError("%s: %s needs tools.native.edit: it writes with edit's paths, max_bytes and backups", settingPath, section)
	}
	settings := &NativeNotebookSettings{Edit: *edit}
	if settings.MaxOutputChars, err = configuredInteger(fields, "max_output_chars", defaultNativeNotebookMaxOutputChars, 0, maxNativeNotebookMaxOutputChars, settingPath, section); err != nil {
		return nil, err
	}
	return settings, nil
}
