package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxJavaScriptSafeInteger int64 = 9007199254740991

type jsonField struct {
	key   string
	value []byte
}

// objectFields splits a JSON object that has not been checked yet. It accepts exactly
// what json.Valid accepts, checking each key and value as it splits them, so the record
// is read once rather than once to validate and again to split.
func objectFields(raw []byte) ([]jsonField, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotJSONObject
	}
	fields, end, err := splitObjectFields(trimmed, true)
	if err != nil || end != len(trimmed) {
		return nil, errNotJSONObject
	}
	return fields, nil
}

var errNotJSONObject = errors.New("expected a JSON object")

// objectFieldsValid splits a JSON object that is already known to be valid.
func objectFieldsValid(raw []byte) ([]jsonField, error) {
	fields, _, err := splitObjectFields(raw, false)
	return fields, err
}

// splitObjectFields splits the object at the start of raw and returns where it ends.
// With validate it checks every key and value fully (a nested value as validJSON does);
// without, it only finds where each one ends, which is enough for bytes already checked.
func splitObjectFields(raw []byte, validate bool) ([]jsonField, int, error) {
	position := skipJSONSpace(raw, 0)
	if position >= len(raw) || raw[position] != '{' {
		return nil, 0, errors.New("expected a JSON object")
	}
	position++
	fields := make([]jsonField, 0, 8)
	// Small objects look keys up linearly; the map is built only once an object
	// grows past objectFieldIndexThreshold fields.
	var indices map[string]int
	for afterComma := false; ; afterComma = true {
		position = skipJSONSpace(raw, position)
		if position >= len(raw) {
			return nil, 0, errors.New("unterminated JSON object")
		}
		// A comma must be followed by another field; only validation needs to say so.
		if raw[position] == '}' && !(validate && afterComma) {
			return fields, position + 1, nil
		}
		keyEnd, err := scanObjectPart(raw, position, validate, validStringEnd, scanJSONString)
		if err != nil {
			return nil, 0, err
		}
		key, err := jsonStringValue(raw[position:keyEnd])
		if err != nil {
			return nil, 0, err
		}
		position = skipJSONSpace(raw, keyEnd)
		if position >= len(raw) || raw[position] != ':' {
			return nil, 0, errors.New("expected colon after JSON object key")
		}
		valueStart := skipJSONSpace(raw, position+1)
		valueEnd, err := scanObjectPart(raw, valueStart, validate, validFieldValueEnd, scanJSONValue)
		if err != nil {
			return nil, 0, err
		}
		field := jsonField{key: key, value: raw[valueStart:valueEnd]}
		if index := indexOfField(fields, indices, key); index >= 0 {
			// JSON.parse keeps the original property position but the final value.
			fields[index].value = field.value
		} else {
			fields = append(fields, field)
			if indices != nil {
				indices[key] = len(fields) - 1
			} else if len(fields) >= objectFieldIndexThreshold {
				indices = make(map[string]int, 2*len(fields))
				for fieldIndex, existing := range fields {
					indices[existing.key] = fieldIndex
				}
			}
		}
		position = skipJSONSpace(raw, valueEnd)
		if position >= len(raw) {
			return nil, 0, errors.New("unterminated JSON object")
		}
		if raw[position] == '}' {
			return fields, position + 1, nil
		}
		if raw[position] != ',' {
			return nil, 0, errors.New("expected comma between JSON object fields")
		}
		position++
	}
}

// scanObjectPart finds the end of the key or value at start: checked with valid when
// validate is set, otherwise only located with scan.
func scanObjectPart(raw []byte, start int, validate bool, valid func([]byte, int) (int, bool), scan func([]byte, int) (int, error)) (int, error) {
	if !validate {
		return scan(raw, start)
	}
	end, ok := valid(raw, start)
	if !ok {
		return 0, errNotJSONObject
	}
	return end, nil
}

// validFieldValueEnd checks a value of a top-level object: one object is already open.
func validFieldValueEnd(raw []byte, start int) (int, bool) { return validValueEnd(raw, start, 1) }

// objectFieldIndexThreshold is the object size at which duplicate-key lookup
// switches from a linear scan to a map. It is an algorithmic cutover, not a limit.
const objectFieldIndexThreshold = 16

func indexOfField(fields []jsonField, indices map[string]int, key string) int {
	if indices != nil {
		if index, found := indices[key]; found {
			return index
		}
		return -1
	}
	for index := range fields {
		if fields[index].key == key {
			return index
		}
	}
	return -1
}

// plainJSONString returns the contents of a JSON string literal whose decoded
// value is byte-for-byte its contents: no escapes and valid UTF-8. Such a string
// is already in canonical form. Anything else reports false so callers decode it.
func plainJSONString(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	inner := raw[1 : len(raw)-1]
	ascii := true
	for _, current := range inner {
		if current == '\\' || current == '"' || current < 0x20 {
			return nil, false
		}
		if current >= 0x80 {
			ascii = false
		}
	}
	if !ascii && !utf8.Valid(inner) {
		return nil, false
	}
	return inner, true
}

// jsonStringValue decodes a JSON string literal like json.Unmarshal into a string.
func jsonStringValue(raw []byte) (string, error) {
	if inner, plain := plainJSONString(raw); plain {
		return string(inner), nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func arrayValues(raw []byte, visit func([]byte) error) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' || !validJSON(trimmed) {
		return errors.New("expected a JSON array")
	}
	position := skipJSONSpace(trimmed, 1)
	if position < len(trimmed) && trimmed[position] == ']' {
		return nil
	}
	for {
		position = skipJSONSpace(trimmed, position)
		end, err := scanJSONValue(trimmed, position)
		if err != nil {
			return err
		}
		if err := visit(trimmed[position:end]); err != nil {
			return err
		}
		position = skipJSONSpace(trimmed, end)
		if position >= len(trimmed) {
			return errors.New("unterminated JSON array")
		}
		if trimmed[position] == ']' {
			return nil
		}
		if trimmed[position] != ',' {
			return errors.New("expected comma between JSON array items")
		}
		position++
	}
}

// scanJSONString finds the end of a string in bytes already checked. It jumps from quote
// to quote with bytes.IndexByte: a quote ends the string unless an odd run of
// backslashes escapes it.
func scanJSONString(raw []byte, start int) (int, error) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, errors.New("expected JSON string")
	}
	for position := start + 1; position < len(raw); {
		quote := bytes.IndexByte(raw[position:], '"')
		if quote < 0 {
			break
		}
		position += quote
		backslashes := 0
		for before := position - 1; before > start && raw[before] == '\\'; before-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return position + 1, nil
		}
		position++
	}
	return 0, errors.New("unterminated JSON string")
}

func scanJSONValue(raw []byte, start int) (int, error) {
	if start >= len(raw) {
		return 0, errors.New("missing JSON value")
	}
	switch raw[start] {
	case '"':
		return scanJSONString(raw, start)
	case '{', '[':
		stack := make([]byte, 0, 8)
		if raw[start] == '{' {
			stack = append(stack, '}')
		} else {
			stack = append(stack, ']')
		}
		for position := start + 1; position < len(raw); position++ {
			switch current := raw[position]; current {
			case '"':
				end, err := scanJSONString(raw, position)
				if err != nil {
					return 0, errors.New("unterminated JSON container")
				}
				position = end - 1
			case '{':
				stack = append(stack, '}')
			case '[':
				stack = append(stack, ']')
			case '}', ']':
				if len(stack) == 0 || stack[len(stack)-1] != current {
					return 0, errors.New("mismatched JSON container")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return position + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated JSON container")
	default:
		position := start
		for position < len(raw) && !isJSONDelimiter(raw[position]) {
			position++
		}
		if position == start {
			return 0, errors.New("invalid JSON value")
		}
		return position, nil
	}
}

func isJSONDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', ',', ']', '}':
		return true
	default:
		return false
	}
}

func skipJSONSpace(raw []byte, position int) int {
	for position < len(raw) {
		switch raw[position] {
		case ' ', '\t', '\r', '\n':
			position++
		default:
			return position
		}
	}
	return position
}

// canonicalJSON mirrors JSON.stringify for object key order, string escaping,
// and IEEE-754 number rendering while retaining nested field insertion order.
func canonicalJSON(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if !validJSON(trimmed) {
		return nil, errors.New("invalid JSON value")
	}
	return canonicalJSONValid(trimmed)
}

func canonicalJSONValid(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing JSON value")
	}
	switch raw[0] {
	case '{':
		fields, err := objectFieldsValid(raw)
		if err != nil {
			return nil, err
		}
		for index := range fields {
			fields[index].value, err = canonicalJSONValid(fields[index].value)
			if err != nil {
				return nil, err
			}
		}
		return encodeJSONObject(fields), nil
	case '[':
		var result bytes.Buffer
		result.WriteByte('[')
		first := true
		err := arrayValuesValid(raw, func(value []byte) error {
			if !first {
				result.WriteByte(',')
			}
			first = false
			canonical, err := canonicalJSONValid(value)
			if err != nil {
				return err
			}
			result.Write(canonical)
			return nil
		})
		if err != nil {
			return nil, err
		}
		result.WriteByte(']')
		return result.Bytes(), nil
	case '"':
		if _, plain := plainJSONString(raw); plain {
			// No escapes and valid UTF-8: canonical form equals the source bytes.
			return append(make([]byte, 0, len(raw)), raw...), nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return appendJSONString(make([]byte, 0, len(raw)+2), value), nil
	case 't', 'f', 'n':
		return append([]byte(nil), raw...), nil
	default:
		return canonicalJSONNumber(raw)
	}
}

func arrayValuesValid(raw []byte, visit func([]byte) error) error {
	position := skipJSONSpace(raw, 1)
	if position < len(raw) && raw[position] == ']' {
		return nil
	}
	for {
		position = skipJSONSpace(raw, position)
		end, err := scanJSONValue(raw, position)
		if err != nil {
			return err
		}
		if err := visit(raw[position:end]); err != nil {
			return err
		}
		position = skipJSONSpace(raw, end)
		if position >= len(raw) {
			return errors.New("unterminated JSON array")
		}
		if raw[position] == ']' {
			return nil
		}
		if raw[position] != ',' {
			return errors.New("expected comma between JSON array items")
		}
		position++
	}
}

func encodeJSONObject(fields []jsonField) []byte {
	ordered := orderJavaScriptKeys(fields)
	result := make([]byte, 0)
	result = append(result, '{')
	for index, field := range ordered {
		if index > 0 {
			result = append(result, ',')
		}
		result = appendJSONString(result, field.key)
		result = append(result, ':')
		result = append(result, field.value...)
	}
	result = append(result, '}')
	return result
}

func orderJavaScriptKeys(fields []jsonField) []jsonField {
	hasIndex := false
	for _, field := range fields {
		if _, isIndex := javascriptArrayIndex(field.key); isIndex {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return fields
	}
	indices := make([]jsonField, 0)
	stringsInOrder := make([]jsonField, 0, len(fields))
	for _, field := range fields {
		if _, isIndex := javascriptArrayIndex(field.key); isIndex {
			indices = append(indices, field)
		} else {
			stringsInOrder = append(stringsInOrder, field)
		}
	}
	sort.SliceStable(indices, func(left, right int) bool {
		leftIndex, _ := javascriptArrayIndex(indices[left].key)
		rightIndex, _ := javascriptArrayIndex(indices[right].key)
		return leftIndex < rightIndex
	})
	return append(indices, stringsInOrder...)
}

func javascriptArrayIndex(key string) (uint64, bool) {
	if key == "" || key[0] < '0' || key[0] > '9' || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseUint(key, 10, 32)
	if err != nil || value == math.MaxUint32 || strconv.FormatUint(value, 10) != key {
		return 0, false
	}
	return value, true
}

func appendJSONString(destination []byte, value string) []byte {
	destination = append(destination, '"')
	for offset := 0; offset < len(value); {
		character, size := utf8.DecodeRuneInString(value[offset:])
		if character == utf8.RuneError && size == 1 {
			destination = append(destination, []byte("�")...)
			offset++
			continue
		}
		switch character {
		case '"':
			destination = append(destination, '\\', '"')
		case '\\':
			destination = append(destination, '\\', '\\')
		case '\b':
			destination = append(destination, '\\', 'b')
		case '\f':
			destination = append(destination, '\\', 'f')
		case '\n':
			destination = append(destination, '\\', 'n')
		case '\r':
			destination = append(destination, '\\', 'r')
		case '\t':
			destination = append(destination, '\\', 't')
		default:
			if character < 0x20 {
				destination = append(destination, '\\', 'u', '0', '0', lowerHex(byte(character)>>4), lowerHex(byte(character)&0x0f))
			} else {
				destination = append(destination, value[offset:offset+size]...)
			}
		}
		offset += size
	}
	return append(destination, '"')
}

func lowerHex(value byte) byte {
	if value < 10 {
		return '0' + value
	}
	return 'a' + value - 10
}

func canonicalJSONNumber(raw []byte) ([]byte, error) {
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, fmt.Errorf("invalid JSON number: %w", err)
	}
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return []byte("null"), nil
	}
	if value == 0 {
		return []byte("0"), nil
	}
	absolute := math.Abs(value)
	if absolute >= 1e-6 && absolute < 1e21 {
		return []byte(strconv.FormatFloat(value, 'f', -1, 64)), nil
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	separator := strings.LastIndexByte(scientific, 'e')
	if separator < 0 {
		return nil, errors.New("could not format JSON number")
	}
	exponent, err := strconv.Atoi(scientific[separator+1:])
	if err != nil {
		return nil, fmt.Errorf("parse JSON number exponent: %w", err)
	}
	if exponent >= 0 {
		scientific = fmt.Sprintf("%se+%d", scientific[:separator], exponent)
	} else {
		scientific = fmt.Sprintf("%se%d", scientific[:separator], exponent)
	}
	return []byte(scientific), nil
}

func safeInteger(raw []byte, positive bool) (int64, bool) {
	value, err := strconv.ParseFloat(string(raw), 64)
	if (err != nil && !errors.Is(err, strconv.ErrRange)) || math.IsInf(value, 0) || math.IsNaN(value) || math.Trunc(value) != value {
		return 0, false
	}
	if positive {
		if value < 1 || value > float64(maxJavaScriptSafeInteger) {
			return 0, false
		}
	} else if value < 0 || value > float64(maxJavaScriptSafeInteger) {
		return 0, false
	}
	return int64(value), true
}

func stringValue(raw []byte) (string, bool) {
	if inner, plain := plainJSONString(raw); plain {
		return string(inner), true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}
