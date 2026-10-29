package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	// Numbers stay json.Number so the encoder writes the literal the server sent:
	// a float64 would round an integer beyond 2^53 and would reject a literal
	// outside float64's range.
	decoder.UseNumber()
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
	return appendOrderedJSONValue(nil, object)
}

// appendOrderedJSONValue encodes a preserved structured-content value onto dst,
// recursing into nested objects and arrays instead of encoding each subtree on its
// own. One buffer for the whole value keeps the cost linear in the size of the
// payload: a nested object no longer re-encodes and re-copies everything below it.
func appendOrderedJSONValue(dst []byte, value any) ([]byte, error) {
	switch typed := value.(type) {
	case orderedJSONObject:
		dst = append(dst, '{')
		for index, field := range typed.fields {
			if index > 0 {
				dst = append(dst, ',')
			}
			name, err := marshalJSON(field.name)
			if err != nil {
				return nil, fmt.Errorf("encode structured content key: %w", err)
			}
			dst = append(dst, name...)
			dst = append(dst, ':')
			dst, err = appendOrderedJSONValue(dst, field.value)
			if err != nil {
				return nil, fmt.Errorf("encode structured content value for %q: %w", field.name, err)
			}
		}
		return append(dst, '}'), nil
	case []any:
		dst = append(dst, '[')
		for index, item := range typed {
			if index > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendOrderedJSONValue(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case json.Number:
		return appendOrderedJSONNumber(dst, typed)
	default:
		encoded, err := marshalJSON(value)
		if err != nil {
			return nil, err
		}
		return append(dst, encoded...), nil
	}
}

// appendOrderedJSONNumber writes the number literal the server sent, so an integer
// beyond 2^53 keeps its digits. A literal above float64's range cannot be
// represented by JavaScript's JSON.parse either: it becomes Infinity there, which
// JSON.stringify writes as null, and null is what this writes too.
func appendOrderedJSONNumber(dst []byte, number json.Number) ([]byte, error) {
	parsed, err := strconv.ParseFloat(number.String(), 64)
	switch {
	case err == nil:
		return append(dst, number...), nil
	case errors.Is(err, strconv.ErrRange) && math.IsInf(parsed, 0):
		return append(dst, "null"...), nil
	case errors.Is(err, strconv.ErrRange):
		// Underflow to zero keeps the digits as sent, like every in-range literal.
		return append(dst, number...), nil
	default:
		return nil, fmt.Errorf("encode structured content number %q: %w", number, err)
	}
}
