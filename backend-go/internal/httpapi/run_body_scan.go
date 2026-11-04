package httpapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// The helpers below walk JSON that json.Valid has already accepted and return sub-slices of it,
// so the run body is scanned in place instead of being copied into json.RawMessage values.

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// skipJSONString returns the index after the string that opens at b[i].
func skipJSONString(b []byte, i int) int {
	j := i + 1
	for {
		k := bytes.IndexByte(b[j:], '"')
		if k < 0 {
			return len(b)
		}
		j += k
		slashes := 0
		for p := j - 1; p > i && b[p] == '\\'; p-- {
			slashes++
		}
		if slashes%2 == 0 {
			return j + 1
		}
		j++
	}
}

// skipJSONValue returns the index after the value that starts at b[i].
func skipJSONValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipJSONString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	}
	for i < len(b) && !strings.ContainsRune(",}] \t\r\n", rune(b[i])) {
		i++
	}
	return i
}

// jsonArrayElements returns the elements of the array b, which must be valid JSON.
func jsonArrayElements(b []byte) [][]byte {
	var elements [][]byte
	i := skipJSONSpace(b, 0) + 1
	for {
		i = skipJSONSpace(b, i)
		if i >= len(b) || b[i] == ']' {
			return elements
		}
		end := skipJSONValue(b, i)
		elements = append(elements, b[i:end])
		i = skipJSONSpace(b, end)
		if i < len(b) && b[i] == ',' {
			i++
		}
	}
}

// eachJSONMember calls visit with the key (quotes included) and the value of every member of
// the object b, which must be valid JSON, in document order.
func eachJSONMember(b []byte, visit func(key, value []byte)) {
	i := skipJSONSpace(b, 0) + 1
	for {
		i = skipJSONSpace(b, i)
		if i >= len(b) || b[i] == '}' {
			return
		}
		keyEnd := skipJSONString(b, i)
		key := b[i:keyEnd]
		i = skipJSONSpace(b, keyEnd) + 1
		i = skipJSONSpace(b, i)
		end := skipJSONValue(b, i)
		visit(key, b[i:end])
		i = skipJSONSpace(b, end)
		if i < len(b) && b[i] == ',' {
			i++
		}
	}
}

// jsonStringValue decodes the JSON string extent (quotes included) the way json.Unmarshal
// does. A string with no escapes and valid UTF-8 needs no decoding, only one copy.
func jsonStringValue(extent []byte) string {
	inner := extent[1 : len(extent)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		return string(inner)
	}
	var text string
	_ = json.Unmarshal(extent, &text)
	return text
}

// jsonStringView is jsonStringValue without the copy when the string needs no decoding. The
// result points into extent, so it is only for callers that neither keep it nor change extent;
// the base64 of an image or file is read this way and never retained.
func jsonStringView(extent []byte) string {
	inner := extent[1 : len(extent)-1]
	if len(inner) > 0 && bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		return unsafe.String(&inner[0], len(inner))
	}
	return jsonStringValue(extent)
}

func firstJSONByte(b []byte) byte {
	if i := skipJSONSpace(b, 0); i < len(b) {
		return b[i]
	}
	return 0
}

// runBodyMembers are the extents of the members of a run request body that the server reads.
type runBodyMembers struct {
	input, model, supports, outputFormat, plan                []byte
	hasInput, hasModel, hasSupports, hasOutputFormat, hasPlan bool
}

// scanRunBodyObject returns the extents of "input", "model", "supports_interaction",
// "output_format" and "plan" in the top-level object b; the last of a repeated key wins, as in a map.
func scanRunBodyObject(b []byte) (members runBodyMembers) {
	eachJSONMember(b, func(key, value []byte) {
		switch jsonStringValue(key) {
		case "input":
			members.input, members.hasInput = value, true
		case "model":
			members.model, members.hasModel = value, true
		case "supports_interaction":
			members.supports, members.hasSupports = value, true
		case "output_format":
			members.outputFormat, members.hasOutputFormat = value, true
		case "plan":
			members.plan, members.hasPlan = value, true
		}
	})
	return
}

// decodeRunInputPart reads one input part like unmarshalling it into a map and into
// runInputPart did: a null part is empty, fields match the struct names case-insensitively,
// and a field of the wrong JSON type, or a part that is not an object, is refused (ok false).
// names are the keys as written, for the unknown-field check.
func decodeRunInputPart(raw []byte) (part runInputPart, names []string, ok bool) {
	switch firstJSONByte(raw) {
	case 'n':
		return part, nil, true
	case '{':
	default:
		return part, nil, false
	}
	ok = true
	eachJSONMember(raw, func(key, value []byte) {
		name := jsonStringValue(key)
		names = append(names, name)
		var target **string
		isType := false
		switch {
		case strings.EqualFold(name, "type"):
			isType = true
		case strings.EqualFold(name, "text"):
			target = &part.Text
		case strings.EqualFold(name, "data"):
			target = &part.Data
		case strings.EqualFold(name, "name"):
			target = &part.Name
		default:
			return
		}
		kind := firstJSONByte(value)
		if kind != '"' && kind != 'n' {
			ok = false
			return
		}
		if isType {
			if kind == '"' {
				part.Type = jsonStringValue(value)
			}
			return
		}
		if kind == 'n' {
			*target = nil
			return
		}
		var text string
		if target == &part.Data {
			text = jsonStringView(value)
		} else {
			text = jsonStringValue(value)
		}
		*target = &text
	})
	return part, names, ok
}
