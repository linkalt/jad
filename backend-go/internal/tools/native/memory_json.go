package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// memoryJSONValue is a minimal ordered JSON tree for JS-compatible stringify.
type memoryJSONKind uint8

const (
	memoryNull memoryJSONKind = iota
	memoryBool
	memoryNumber
	memoryString
	memoryArray
	memoryObject
)

type memoryJSONPair struct {
	key   string
	value *memoryJSONValue
}
type memoryJSONValue struct {
	kind   memoryJSONKind
	bool   bool
	text   string
	array  []*memoryJSONValue
	object []memoryJSONPair
}

func memoryObjectValue(pairs []memoryJSONPair) *memoryJSONValue {
	return &memoryJSONValue{kind: memoryObject, object: pairs}
}
func memoryArrayValue(values []*memoryJSONValue) *memoryJSONValue {
	return &memoryJSONValue{kind: memoryArray, array: values}
}
func memoryArrayValueFromItems(items []MemoryItem) *memoryJSONValue {
	values := make([]*memoryJSONValue, 0, len(items))
	for _, item := range items {
		values = append(values, memoryValueFromGo(item))
	}
	return memoryArrayValue(values)
}

func memoryValueFromGo(value any) *memoryJSONValue {
	switch typed := value.(type) {
	case nil:
		return &memoryJSONValue{kind: memoryNull}
	case bool:
		return &memoryJSONValue{kind: memoryBool, bool: typed}
	case string:
		return &memoryJSONValue{kind: memoryString, text: typed}
	case json.Number:
		return &memoryJSONValue{kind: memoryNumber, text: typed.String()}
	case float64:
		return &memoryJSONValue{kind: memoryNumber, text: strconv.FormatFloat(typed, 'g', -1, 64)}
	case float32:
		return &memoryJSONValue{kind: memoryNumber, text: strconv.FormatFloat(float64(typed), 'g', -1, 32)}
	case int:
		return &memoryJSONValue{kind: memoryNumber, text: strconv.Itoa(typed)}
	case int64:
		return &memoryJSONValue{kind: memoryNumber, text: strconv.FormatInt(typed, 10)}
	case uint64:
		return &memoryJSONValue{kind: memoryNumber, text: strconv.FormatUint(typed, 10)}
	case []any:
		array := make([]*memoryJSONValue, 0, len(typed))
		for _, entry := range typed {
			array = append(array, memoryValueFromGo(entry))
		}
		return memoryArrayValue(array)
	case MemoryItem:
		data := typed.value
		if data == nil {
			data = memoryValueFromGo(typed.Data)
		}
		return memoryObjectValue([]memoryJSONPair{
			{key: "ref", value: memoryValueFromGo(typed.Ref)},
			{key: "etag", value: memoryValueFromGo(typed.ETag)},
			{key: "data", value: data},
		})
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]memoryJSONPair, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, memoryJSONPair{key: key, value: memoryValueFromGo(typed[key])})
		}
		return memoryObjectValue(pairs)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return &memoryJSONValue{kind: memoryNull}
		}
		value, err := parseMemoryJSON(encoded)
		if err != nil {
			return &memoryJSONValue{kind: memoryNull}
		}
		return value
	}
}

func parseMemoryJSON(data []byte) (*memoryJSONValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := parseMemoryJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("unexpected trailing JSON data")
	}
	return value, nil
}

// parseStoredMemoryJSON parses a stored record. A record that fails to parse is read
// through its undone text only when memoryLegacyEscapeText proves it was written with
// the doubled escapes.
func parseStoredMemoryJSON(text []byte) (*memoryJSONValue, error) {
	value, err := parseMemoryJSON(text)
	if err == nil {
		return value, nil
	}
	if _, legacy, ok := memoryLegacyEscapeText(text); ok {
		return legacy, nil
	}
	return nil, err
}

// memoryLegacyEscapeText proves a record was written by quoteMemoryString before its
// fix. The old writer doubled every backslash in the correct encoding, so undoing that
// is one ReplaceAll. The proof needs all of these: the stored text is invalid JSON, the
// undone text parses to an object, and the undone text is the canonical pretty form of
// that object. A record that is valid JSON is never reported as legacy, because an old
// backslash, newline or tab record is valid JSON with a wrong value and cannot be told
// apart from a correct record.
func memoryLegacyEscapeText(text []byte) ([]byte, *memoryJSONValue, bool) {
	if _, err := parseMemoryJSON(text); err == nil {
		return nil, nil, false
	}
	undone := bytes.ReplaceAll(text, []byte(`\\`), []byte(`\`))
	value, err := parseMemoryJSON(undone)
	if err != nil || value.kind != memoryObject {
		return nil, nil, false
	}
	canonical, err := memoryJSONPretty(value)
	if err != nil || canonical != string(undone) {
		return nil, nil, false
	}
	return undone, value, true
}

func parseMemoryJSONValue(decoder *json.Decoder) (*memoryJSONValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			pairs := make([]memoryJSONPair, 0)
			positions := make(map[string]int)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid JSON object key")
				}
				value, err := parseMemoryJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				if position, exists := positions[key]; exists {
					pairs[position].value = value
				} else {
					positions[key] = len(pairs)
					pairs = append(pairs, memoryJSONPair{key: key, value: value})
				}
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return memoryObjectValue(memoryJSObjectOrder(pairs)), nil
		case '[':
			values := make([]*memoryJSONValue, 0)
			for decoder.More() {
				value, err := parseMemoryJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return memoryArrayValue(values), nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		return &memoryJSONValue{kind: memoryNumber, text: typed.String()}, nil
	case string:
		return &memoryJSONValue{kind: memoryString, text: typed}, nil
	case bool:
		return &memoryJSONValue{kind: memoryBool, bool: typed}, nil
	case nil:
		return &memoryJSONValue{kind: memoryNull}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func memoryJSObjectOrder(pairs []memoryJSONPair) []memoryJSONPair {
	type indexedPair struct {
		pair  memoryJSONPair
		index uint64
	}
	indices := make([]indexedPair, 0)
	other := make([]memoryJSONPair, 0, len(pairs))
	for _, pair := range pairs {
		if index, ok := memoryArrayIndex(pair.key); ok {
			indices = append(indices, indexedPair{pair: pair, index: index})
		} else {
			other = append(other, pair)
		}
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i].index < indices[j].index })
	ordered := make([]memoryJSONPair, 0, len(pairs))
	for _, entry := range indices {
		ordered = append(ordered, entry.pair)
	}
	return append(ordered, other...)
}

func memoryArrayIndex(key string) (uint64, bool) {
	if key == "0" {
		return 0, true
	}
	if key == "" || key[0] == '0' {
		return 0, false
	}
	index, err := strconv.ParseUint(key, 10, 32)
	if err != nil || index > (1<<32)-2 || strconv.FormatUint(index, 10) != key {
		return 0, false
	}
	return index, true
}

func memoryGoMap(value *memoryJSONValue) map[string]any {
	out := make(map[string]any, len(value.object))
	for _, pair := range value.object {
		out[pair.key] = memoryGoValue(pair.value)
	}
	return out
}
func memoryGoValue(value *memoryJSONValue) any {
	switch value.kind {
	case memoryNull:
		return nil
	case memoryBool:
		return value.bool
	case memoryNumber:
		return json.Number(value.text)
	case memoryString:
		return value.text
	case memoryArray:
		out := make([]any, 0, len(value.array))
		for _, entry := range value.array {
			out = append(out, memoryGoValue(entry))
		}
		return out
	case memoryObject:
		return memoryGoMap(value)
	default:
		return nil
	}
}

func memoryJSONDepth(value *memoryJSONValue) int64 {
	if value == nil || (value.kind != memoryArray && value.kind != memoryObject) {
		return 0
	}
	var child int64
	if value.kind == memoryArray {
		for _, entry := range value.array {
			child = max(child, memoryJSONDepth(entry))
		}
	} else {
		for _, pair := range value.object {
			child = max(child, memoryJSONDepth(pair.value))
		}
	}
	return child + 1
}

func memoryJSONCompact(value *memoryJSONValue) ([]byte, error) {
	var output bytes.Buffer
	if err := writeMemoryJSON(&output, value, -1, 0); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
func memoryJSONPretty(value *memoryJSONValue) (string, error) {
	var output bytes.Buffer
	if err := writeMemoryJSON(&output, value, 2, 0); err != nil {
		return "", err
	}
	return output.String() + "\n", nil
}
func writeMemoryJSON(output *bytes.Buffer, value *memoryJSONValue, indent, depth int) error {
	if value == nil {
		output.WriteString("null")
		return nil
	}
	switch value.kind {
	case memoryNull:
		output.WriteString("null")
	case memoryBool:
		if value.bool {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case memoryNumber:
		output.WriteString(memoryJSNumber(value.text))
	case memoryString:
		encoded, err := quoteMemoryString(value.text)
		if err != nil {
			return err
		}
		output.Write(encoded)
	case memoryArray:
		output.WriteByte('[')
		for index, entry := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			if indent >= 0 {
				output.WriteByte('\n')
				writeMemoryIndent(output, indent, depth+1)
			}
			if err := writeMemoryJSON(output, entry, indent, depth+1); err != nil {
				return err
			}
		}
		if len(value.array) > 0 && indent >= 0 {
			output.WriteByte('\n')
			writeMemoryIndent(output, indent, depth)
		}
		output.WriteByte(']')
	case memoryObject:
		output.WriteByte('{')
		for index, pair := range value.object {
			if index > 0 {
				output.WriteByte(',')
			}
			if indent >= 0 {
				output.WriteByte('\n')
				writeMemoryIndent(output, indent, depth+1)
			}
			key, err := quoteMemoryString(pair.key)
			if err != nil {
				return err
			}
			output.Write(key)
			if indent >= 0 {
				output.WriteString(": ")
			} else {
				output.WriteByte(':')
			}
			if err := writeMemoryJSON(output, pair.value, indent, depth+1); err != nil {
				return err
			}
		}
		if len(value.object) > 0 && indent >= 0 {
			output.WriteByte('\n')
			writeMemoryIndent(output, indent, depth)
		}
		output.WriteByte('}')
	default:
		return errors.New("invalid memory JSON value")
	}
	return nil
}
func writeMemoryIndent(output *bytes.Buffer, indent, depth int) {
	for range indent * depth {
		output.WriteByte(' ')
	}
}
func quoteMemoryString(value string) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if character < 0x20 {
				fmt.Fprintf(&output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
	return output.Bytes(), nil
}
func memoryJSNumber(value string) string {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return "null"
	}
	if number == 0 {
		return "0"
	}
	abs := math.Abs(number)
	if abs >= 1e21 || abs < 1e-6 {
		formatted := strconv.FormatFloat(number, 'e', -1, 64)
		parts := strings.Split(formatted, "e")
		if len(parts) != 2 {
			return formatted
		}
		exponent, _ := strconv.Atoi(parts[1])
		sign := "+"
		if exponent < 0 {
			sign = "-"
			exponent = -exponent
		}
		return parts[0] + "e" + sign + strconv.Itoa(exponent)
	}
	return strconv.FormatFloat(number, 'f', -1, 64)
}
func memoryJSONCanonical(value *memoryJSONValue) (string, error) {
	canonical := memorySortJSON(value)
	encoded, err := memoryJSONCompact(canonical)
	return string(encoded), err
}
func memorySortJSON(value *memoryJSONValue) *memoryJSONValue {
	if value == nil {
		return &memoryJSONValue{kind: memoryNull}
	}
	if value.kind == memoryArray {
		array := make([]*memoryJSONValue, len(value.array))
		for index, entry := range value.array {
			array[index] = memorySortJSON(entry)
		}
		return memoryArrayValue(array)
	}
	if value.kind == memoryObject {
		pairs := append([]memoryJSONPair(nil), value.object...)
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
		for index := range pairs {
			pairs[index].value = memorySortJSON(pairs[index].value)
		}
		return memoryObjectValue(pairs)
	}
	return value
}
