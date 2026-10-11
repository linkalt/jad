package sessionfs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// PathProfileInfo describes the active and available profiles of a session.
type PathProfileInfo struct {
	ActiveProfile string
	PathProfiles  []string
}

// PathProfileUpdate is the persisted result of changing the active path profile.
type PathProfileUpdate struct {
	ActiveProfile string
	PathProfiles  []string
	Changed       bool
}

// ModelDefaultUpdate is the persisted model selection for one path profile.
type ModelDefaultUpdate struct {
	ActiveProfile string
	PathProfiles  []string
	ModelDefault  string
	Changed       bool
}

func rawObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, configError("%s must be a JSON object", path)
	}
	return fields, nil
}

func marshalOrderedObject(fields map[string]json.RawMessage, preferredOrder []string) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('{')
	written := make(map[string]struct{}, len(fields))
	first := true
	appendField := func(name string, value json.RawMessage) error {
		encodedName, err := json.Marshal(name)
		if err != nil {
			return err
		}
		if !first {
			output.WriteByte(',')
		}
		first = false
		output.Write(encodedName)
		output.WriteByte(':')
		output.Write(value)
		written[name] = struct{}{}
		return nil
	}
	for _, name := range preferredOrder {
		if value, exists := fields[name]; exists {
			if err := appendField(name, value); err != nil {
				return nil, err
			}
		}
	}
	remainder := make([]string, 0, len(fields)-len(written))
	for name := range fields {
		if _, exists := written[name]; !exists {
			remainder = append(remainder, name)
		}
	}
	sort.Strings(remainder)
	for _, name := range remainder {
		if err := appendField(name, fields[name]); err != nil {
			return nil, err
		}
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func writeSessionSettingsAtomic(path string, setting map[string]json.RawMessage, syncWrite bool) (result error) {
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if syncWrite {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
