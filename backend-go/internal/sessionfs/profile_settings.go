package sessionfs

import (
	"bytes"
	"encoding/json"
	"sort"
)

// ProfileInfo describes the active and available profiles of a session.
type ProfileInfo struct {
	ActiveProfile string
	Profiles      []string
}

// ModelDefaultUpdate is the persisted model selection for one path profile.
type ModelDefaultUpdate struct {
	ActiveProfile string
	Profiles      []string
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
