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

// recordsJSONValue is a minimal ordered JSON tree for JS-compatible stringify.
type recordsJSONKind uint8

const (
	recordsNull recordsJSONKind = iota
	recordsBool
	recordsNumber
	recordsString
	recordsArray
	recordsObject
)

type recordsJSONPair struct {
	key   string
	value *recordsJSONValue
}
type recordsJSONValue struct {
	kind   recordsJSONKind
	bool   bool
	text   string
	array  []*recordsJSONValue
	object []recordsJSONPair
}

func recordsObjectValue(pairs []recordsJSONPair) *recordsJSONValue {
	return &recordsJSONValue{kind: recordsObject, object: pairs}
}
func recordsArrayValue(values []*recordsJSONValue) *recordsJSONValue {
	return &recordsJSONValue{kind: recordsArray, array: values}
}
func recordsArrayValueFromItems(items []RecordsItem) *recordsJSONValue {
	values := make([]*recordsJSONValue, 0, len(items))
	for _, item := range items {
		values = append(values, recordsValueFromGo(item))
	}
	return recordsArrayValue(values)
}

func recordsValueFromGo(value any) *recordsJSONValue {
	switch typed := value.(type) {
	case nil:
		return &recordsJSONValue{kind: recordsNull}
	case bool:
		return &recordsJSONValue{kind: recordsBool, bool: typed}
	case string:
		return &recordsJSONValue{kind: recordsString, text: typed}
	case json.Number:
		return &recordsJSONValue{kind: recordsNumber, text: typed.String()}
	case float64:
		return &recordsJSONValue{kind: recordsNumber, text: strconv.FormatFloat(typed, 'g', -1, 64)}
	case float32:
		return &recordsJSONValue{kind: recordsNumber, text: strconv.FormatFloat(float64(typed), 'g', -1, 32)}
	case int:
		return &recordsJSONValue{kind: recordsNumber, text: strconv.Itoa(typed)}
	case int64:
		return &recordsJSONValue{kind: recordsNumber, text: strconv.FormatInt(typed, 10)}
	case uint64:
		return &recordsJSONValue{kind: recordsNumber, text: strconv.FormatUint(typed, 10)}
	case []any:
		array := make([]*recordsJSONValue, 0, len(typed))
		for _, entry := range typed {
			array = append(array, recordsValueFromGo(entry))
		}
		return recordsArrayValue(array)
	case RecordsItem:
		data := typed.value
		if data == nil {
			data = recordsValueFromGo(typed.Data)
		}
		return recordsObjectValue([]recordsJSONPair{
			{key: "ref", value: recordsValueFromGo(typed.Ref)},
			{key: "etag", value: recordsValueFromGo(typed.ETag)},
			{key: "data", value: data},
		})
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]recordsJSONPair, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, recordsJSONPair{key: key, value: recordsValueFromGo(typed[key])})
		}
		return recordsObjectValue(pairs)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return &recordsJSONValue{kind: recordsNull}
		}
		value, err := parseRecordsJSON(encoded)
		if err != nil {
			return &recordsJSONValue{kind: recordsNull}
		}
		return value
	}
}

func parseRecordsJSON(data []byte) (*recordsJSONValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := parseRecordsJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("unexpected trailing JSON data")
	}
	return value, nil
}

// parseStoredRecordsJSON parses a stored record. A record that fails to parse is read
// through its undone text only when recordsLegacyEscapeText proves it was written with
// the doubled escapes.
func parseStoredRecordsJSON(text []byte) (*recordsJSONValue, error) {
	value, err := parseRecordsJSON(text)
	if err == nil {
		return value, nil
	}
	if _, legacy, ok := recordsLegacyEscapeText(text); ok {
		return legacy, nil
	}
	return nil, err
}

// recordsLegacyEscapeText proves a record was written by quoteRecordsString before its
// fix. The old writer doubled every backslash in the correct encoding, so undoing that
// is one ReplaceAll. The proof needs all of these: the stored text is invalid JSON, the
// undone text parses to an object, and the undone text is the canonical pretty form of
// that object. A record that is valid JSON is never reported as legacy, because an old
// backslash, newline or tab record is valid JSON with a wrong value and cannot be told
// apart from a correct record.
func recordsLegacyEscapeText(text []byte) ([]byte, *recordsJSONValue, bool) {
	if _, err := parseRecordsJSON(text); err == nil {
		return nil, nil, false
	}
	undone := bytes.ReplaceAll(text, []byte(`\\`), []byte(`\`))
	value, err := parseRecordsJSON(undone)
	if err != nil || value.kind != recordsObject {
		return nil, nil, false
	}
	canonical, err := recordsJSONPretty(value)
	if err != nil || canonical != string(undone) {
		return nil, nil, false
	}
	return undone, value, true
}

func parseRecordsJSONValue(decoder *json.Decoder) (*recordsJSONValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			pairs := make([]recordsJSONPair, 0)
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
				value, err := parseRecordsJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				if position, exists := positions[key]; exists {
					pairs[position].value = value
				} else {
					positions[key] = len(pairs)
					pairs = append(pairs, recordsJSONPair{key: key, value: value})
				}
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return recordsObjectValue(recordsJSObjectOrder(pairs)), nil
		case '[':
			values := make([]*recordsJSONValue, 0)
			for decoder.More() {
				value, err := parseRecordsJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return recordsArrayValue(values), nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		return &recordsJSONValue{kind: recordsNumber, text: typed.String()}, nil
	case string:
		return &recordsJSONValue{kind: recordsString, text: typed}, nil
	case bool:
		return &recordsJSONValue{kind: recordsBool, bool: typed}, nil
	case nil:
		return &recordsJSONValue{kind: recordsNull}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func recordsJSObjectOrder(pairs []recordsJSONPair) []recordsJSONPair {
	type indexedPair struct {
		pair  recordsJSONPair
		index uint64
	}
	indices := make([]indexedPair, 0)
	other := make([]recordsJSONPair, 0, len(pairs))
	for _, pair := range pairs {
		if index, ok := recordsArrayIndex(pair.key); ok {
			indices = append(indices, indexedPair{pair: pair, index: index})
		} else {
			other = append(other, pair)
		}
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i].index < indices[j].index })
	ordered := make([]recordsJSONPair, 0, len(pairs))
	for _, entry := range indices {
		ordered = append(ordered, entry.pair)
	}
	return append(ordered, other...)
}

func recordsArrayIndex(key string) (uint64, bool) {
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

func recordsGoMap(value *recordsJSONValue) map[string]any {
	out := make(map[string]any, len(value.object))
	for _, pair := range value.object {
		out[pair.key] = recordsGoValue(pair.value)
	}
	return out
}
func recordsGoValue(value *recordsJSONValue) any {
	switch value.kind {
	case recordsNull:
		return nil
	case recordsBool:
		return value.bool
	case recordsNumber:
		return json.Number(value.text)
	case recordsString:
		return value.text
	case recordsArray:
		out := make([]any, 0, len(value.array))
		for _, entry := range value.array {
			out = append(out, recordsGoValue(entry))
		}
		return out
	case recordsObject:
		return recordsGoMap(value)
	default:
		return nil
	}
}

func recordsJSONDepth(value *recordsJSONValue) int64 {
	if value == nil || (value.kind != recordsArray && value.kind != recordsObject) {
		return 0
	}
	var child int64
	if value.kind == recordsArray {
		for _, entry := range value.array {
			child = max(child, recordsJSONDepth(entry))
		}
	} else {
		for _, pair := range value.object {
			child = max(child, recordsJSONDepth(pair.value))
		}
	}
	return child + 1
}

func recordsJSONCompact(value *recordsJSONValue) ([]byte, error) {
	var output bytes.Buffer
	if err := writeRecordsJSON(&output, value, -1, 0); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
func recordsJSONPretty(value *recordsJSONValue) (string, error) {
	var output bytes.Buffer
	if err := writeRecordsJSON(&output, value, 2, 0); err != nil {
		return "", err
	}
	return output.String() + "\n", nil
}
func writeRecordsJSON(output *bytes.Buffer, value *recordsJSONValue, indent, depth int) error {
	if value == nil {
		output.WriteString("null")
		return nil
	}
	switch value.kind {
	case recordsNull:
		output.WriteString("null")
	case recordsBool:
		if value.bool {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case recordsNumber:
		output.WriteString(recordsJSNumber(value.text))
	case recordsString:
		encoded, err := quoteRecordsString(value.text)
		if err != nil {
			return err
		}
		output.Write(encoded)
	case recordsArray:
		output.WriteByte('[')
		for index, entry := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			if indent >= 0 {
				output.WriteByte('\n')
				writeRecordsIndent(output, indent, depth+1)
			}
			if err := writeRecordsJSON(output, entry, indent, depth+1); err != nil {
				return err
			}
		}
		if len(value.array) > 0 && indent >= 0 {
			output.WriteByte('\n')
			writeRecordsIndent(output, indent, depth)
		}
		output.WriteByte(']')
	case recordsObject:
		output.WriteByte('{')
		for index, pair := range value.object {
			if index > 0 {
				output.WriteByte(',')
			}
			if indent >= 0 {
				output.WriteByte('\n')
				writeRecordsIndent(output, indent, depth+1)
			}
			key, err := quoteRecordsString(pair.key)
			if err != nil {
				return err
			}
			output.Write(key)
			if indent >= 0 {
				output.WriteString(": ")
			} else {
				output.WriteByte(':')
			}
			if err := writeRecordsJSON(output, pair.value, indent, depth+1); err != nil {
				return err
			}
		}
		if len(value.object) > 0 && indent >= 0 {
			output.WriteByte('\n')
			writeRecordsIndent(output, indent, depth)
		}
		output.WriteByte('}')
	default:
		return errors.New("invalid records JSON value")
	}
	return nil
}
func writeRecordsIndent(output *bytes.Buffer, indent, depth int) {
	for range indent * depth {
		output.WriteByte(' ')
	}
}
func quoteRecordsString(value string) ([]byte, error) {
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
func recordsJSNumber(value string) string {
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
func recordsJSONCanonical(value *recordsJSONValue) (string, error) {
	canonical := recordsSortJSON(value)
	encoded, err := recordsJSONCompact(canonical)
	return string(encoded), err
}
func recordsSortJSON(value *recordsJSONValue) *recordsJSONValue {
	if value == nil {
		return &recordsJSONValue{kind: recordsNull}
	}
	if value.kind == recordsArray {
		array := make([]*recordsJSONValue, len(value.array))
		for index, entry := range value.array {
			array[index] = recordsSortJSON(entry)
		}
		return recordsArrayValue(array)
	}
	if value.kind == recordsObject {
		pairs := append([]recordsJSONPair(nil), value.object...)
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
		for index := range pairs {
			pairs[index].value = recordsSortJSON(pairs[index].value)
		}
		return recordsObjectValue(pairs)
	}
	return value
}
