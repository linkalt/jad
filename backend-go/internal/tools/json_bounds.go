package tools

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// DraftJSONBytes returns the UTF-8 byte size of a compact JSON payload after
// parsing and stringifying it, matching JSON.stringify at the tool boundary.
func DraftJSONBytes(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	normalized, err := normalizeJSONStringifyNumbers(value)
	if err != nil {
		return 0, err
	}
	compact, err := MarshalJSON(normalized)
	if err != nil {
		return 0, err
	}
	return int64(len(compact)), nil
}

func replaceJSONStringSeparators(raw []byte) []byte {
	result := make([]byte, 0, len(raw))
	inString := false
	escaped := false
	for index := 0; index < len(raw); {
		current := raw[index]
		if !inString {
			result = append(result, current)
			if current == '"' {
				inString = true
			}
			index++
			continue
		}
		if escaped {
			result = append(result, current)
			escaped = false
			index++
			continue
		}
		if current == '\\' {
			switch {
			case bytes.HasPrefix(raw[index:], []byte(`\u2028`)):
				result = append(result, []byte("\u2028")...)
				index += len(`\u2028`)
			case bytes.HasPrefix(raw[index:], []byte(`\u2029`)):
				result = append(result, []byte("\u2029")...)
				index += len(`\u2029`)
			default:
				result = append(result, current)
				escaped = true
				index++
			}
			continue
		}
		result = append(result, current)
		if current == '"' {
			inString = false
		}
		index++
	}
	return result
}

func normalizeJSONStringifyNumbers(value any) (any, error) {
	switch current := value.(type) {
	case json.Number:
		number, err := strconv.ParseFloat(string(current), 64)
		if err != nil {
			if math.IsInf(number, 0) {
				return nil, nil
			}
			if number == 0 {
				return float64(0), nil
			}
			return nil, err
		}
		if number == 0 {
			return float64(0), nil
		}
		if math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, nil
		}
		return number, nil
	case map[string]any:
		normalized := make(map[string]any, len(current))
		for key, child := range current {
			value, err := normalizeJSONStringifyNumbers(child)
			if err != nil {
				return nil, err
			}
			normalized[key] = value
		}
		return normalized, nil
	case []any:
		normalized := make([]any, len(current))
		for index, child := range current {
			value, err := normalizeJSONStringifyNumbers(child)
			if err != nil {
				return nil, err
			}
			normalized[index] = value
		}
		return normalized, nil
	default:
		return value, nil
	}
}

// DraftJSONDepth counts nested JSON containers; a scalar has depth zero.
func DraftJSONDepth(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	return draftValueDepth(value), nil
}

func draftValueDepth(value any) int64 {
	switch current := value.(type) {
	case map[string]any:
		deepest := int64(1)
		for _, child := range current {
			if depth := draftValueDepth(child) + 1; depth > deepest {
				deepest = depth
			}
		}
		return deepest
	case []any:
		deepest := int64(1)
		for _, child := range current {
			if depth := draftValueDepth(child) + 1; depth > deepest {
				deepest = depth
			}
		}
		return deepest
	default:
		return 0
	}
}
