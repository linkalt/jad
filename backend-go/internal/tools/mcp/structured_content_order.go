package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
)

type orderedJSONField struct {
	name  string
	value any
}

type orderedJSONObject struct {
	fields []orderedJSONField
}

// preserveStructuredContentOrder reconstructs a JSON value from its wire form,
// keeping object-key order for JavaScript-compatible structured-content text.
func preserveStructuredContentOrder(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	value, err := readOrderedJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected data after structured content")
		}
		return nil, fmt.Errorf("read structured content ending: %w", err)
	}
	return value, nil
}

func readOrderedJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}

	switch delim {
	case '{':
		fields := make([]orderedJSONField, 0)
		positions := make(map[string]int)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("read structured content object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("structured content object key has type %T", keyToken)
			}
			value, err := readOrderedJSONValue(decoder)
			if err != nil {
				return nil, fmt.Errorf("read structured content field %q: %w", key, err)
			}
			if position, exists := positions[key]; exists {
				// JSON.parse keeps the first insertion position but the last value
				// when an object contains a duplicate property name.
				fields[position].value = value
				continue
			}
			positions[key] = len(fields)
			fields = append(fields, orderedJSONField{name: key, value: value})
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("close structured content object: %w", err)
		}
		sortJavaScriptObjectKeys(fields)
		return orderedJSONObject{fields: fields}, nil
	case '[':
		values := make([]any, 0)
		for decoder.More() {
			value, err := readOrderedJSONValue(decoder)
			if err != nil {
				return nil, fmt.Errorf("read structured content array item: %w", err)
			}
			values = append(values, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("close structured content array: %w", err)
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unexpected structured content delimiter %q", delim)
	}
}

func sortJavaScriptObjectKeys(fields []orderedJSONField) {
	sort.SliceStable(fields, func(i, j int) bool {
		left, leftIsIndex := javascriptArrayIndex(fields[i].name)
		right, rightIsIndex := javascriptArrayIndex(fields[j].name)
		if leftIsIndex != rightIsIndex {
			return leftIsIndex
		}
		return leftIsIndex && left < right
	})
}

func javascriptArrayIndex(name string) (uint64, bool) {
	if name == "" || len(name) > 1 && name[0] == '0' {
		return 0, false
	}
	for index := 0; index < len(name); index++ {
		if name[index] < '0' || name[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(name, 10, 32)
	if err != nil || value == 1<<32-1 || strconv.FormatUint(value, 10) != name {
		return 0, false
	}
	return value, true
}

func (object orderedJSONObject) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, field := range object.fields {
		if index > 0 {
			buffer.WriteByte(',')
		}
		name, err := marshalJSON(field.name)
		if err != nil {
			return nil, fmt.Errorf("encode structured content key: %w", err)
		}
		value, err := marshalJSON(field.value)
		if err != nil {
			return nil, fmt.Errorf("encode structured content value for %q: %w", field.name, err)
		}
		buffer.Write(name)
		buffer.WriteByte(':')
		buffer.Write(value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}
