package transcript

import "encoding/binary"

// maxJSONNestingDepth is encoding/json's nesting limit: json.Valid refuses a value with
// more open objects and arrays than this, so validJSON refuses it too.
const maxJSONNestingDepth = 10000

// validJSON reports what json.Valid reports: raw is one JSON value, with only JSON
// whitespace around it. It reads raw once, in a direct loop instead of encoding/json's
// per-byte state machine, and checks string bodies eight bytes at a time.
func validJSON(raw []byte) bool {
	end, ok := validValueEnd(raw, skipJSONSpace(raw, 0), 0)
	return ok && skipJSONSpace(raw, end) == len(raw)
}

// validValueEnd checks the JSON value that starts at start and returns where it ends.
// depth is the number of objects and arrays already open around it.
func validValueEnd(raw []byte, start, depth int) (int, bool) {
	if start >= len(raw) {
		return 0, false
	}
	switch current := raw[start]; current {
	case '{':
		return validObjectEnd(raw, start, depth+1)
	case '[':
		return validArrayEnd(raw, start, depth+1)
	case '"':
		return validStringEnd(raw, start)
	case 't':
		return validLiteralEnd(raw, start, "true")
	case 'f':
		return validLiteralEnd(raw, start, "false")
	case 'n':
		return validLiteralEnd(raw, start, "null")
	default:
		if current == '-' || isDigit(current) {
			return validNumberEnd(raw, start)
		}
		return 0, false
	}
}

func validObjectEnd(raw []byte, start, depth int) (int, bool) {
	if depth > maxJSONNestingDepth {
		return 0, false
	}
	position := skipJSONSpace(raw, start+1)
	if position < len(raw) && raw[position] == '}' {
		return position + 1, true
	}
	for {
		keyEnd, ok := validStringEnd(raw, position)
		if !ok {
			return 0, false
		}
		position = skipJSONSpace(raw, keyEnd)
		if position >= len(raw) || raw[position] != ':' {
			return 0, false
		}
		valueEnd, ok := validValueEnd(raw, skipJSONSpace(raw, position+1), depth)
		if !ok {
			return 0, false
		}
		position = skipJSONSpace(raw, valueEnd)
		if position >= len(raw) {
			return 0, false
		}
		switch raw[position] {
		case '}':
			return position + 1, true
		case ',':
			position = skipJSONSpace(raw, position+1)
		default:
			return 0, false
		}
	}
}

func validArrayEnd(raw []byte, start, depth int) (int, bool) {
	if depth > maxJSONNestingDepth {
		return 0, false
	}
	position := skipJSONSpace(raw, start+1)
	if position < len(raw) && raw[position] == ']' {
		return position + 1, true
	}
	for {
		valueEnd, ok := validValueEnd(raw, position, depth)
		if !ok {
			return 0, false
		}
		position = skipJSONSpace(raw, valueEnd)
		if position >= len(raw) {
			return 0, false
		}
		switch raw[position] {
		case ']':
			return position + 1, true
		case ',':
			position = skipJSONSpace(raw, position+1)
		default:
			return 0, false
		}
	}
}

// Word masks for checking eight string bytes at once.
const (
	everyByte     uint64 = 0x0101010101010101
	everyHighBit  uint64 = 0x8080808080808080
	quoteBytes           = everyByte * '"'
	backslashes          = everyByte * '\\'
	controlCutoff        = everyByte * 0x20
)

// wordNeedsAttention reports whether one of the eight bytes is a quote, a backslash or
// a control byte (below 0x20). It never misses one; a word it flags is checked byte by byte.
func wordNeedsAttention(word uint64) bool {
	quote := word ^ quoteBytes
	backslash := word ^ backslashes
	return ((quote-everyByte)&^quote|(backslash-everyByte)&^backslash|(word-controlCutoff)&^word)&everyHighBit != 0
}

// validStringEnd checks the JSON string that starts at start as encoding/json does: no
// control byte, only the defined escapes, four hex digits after \u. Like encoding/json,
// it accepts any other byte, including bytes that are not valid UTF-8.
func validStringEnd(raw []byte, start int) (int, bool) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, false
	}
	position := start + 1
	for {
		for position+8 <= len(raw) && !wordNeedsAttention(binary.LittleEndian.Uint64(raw[position:])) {
			position += 8
		}
		if position >= len(raw) {
			return 0, false
		}
		switch current := raw[position]; {
		case current == '"':
			return position + 1, true
		case current == '\\':
			position++
			if position >= len(raw) {
				return 0, false
			}
			switch raw[position] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				position++
			case 'u':
				if position+4 >= len(raw) {
					return 0, false
				}
				for _, digit := range raw[position+1 : position+5] {
					if !isHexDigit(digit) {
						return 0, false
					}
				}
				position += 5
			default:
				return 0, false
			}
		case current < 0x20:
			return 0, false
		default:
			position++
		}
	}
}

// validNumberEnd checks -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? and returns where
// it ends. What may follow it is up to the enclosing value, as in encoding/json.
func validNumberEnd(raw []byte, start int) (int, bool) {
	position := start
	if raw[position] == '-' {
		position++
	}
	if position >= len(raw) || !isDigit(raw[position]) {
		return 0, false
	}
	if raw[position] == '0' {
		position++
	} else {
		position = skipDigits(raw, position)
	}
	if position < len(raw) && raw[position] == '.' {
		position++
		if position >= len(raw) || !isDigit(raw[position]) {
			return 0, false
		}
		position = skipDigits(raw, position)
	}
	if position < len(raw) && (raw[position] == 'e' || raw[position] == 'E') {
		position++
		if position < len(raw) && (raw[position] == '+' || raw[position] == '-') {
			position++
		}
		if position >= len(raw) || !isDigit(raw[position]) {
			return 0, false
		}
		position = skipDigits(raw, position)
	}
	return position, true
}

func validLiteralEnd(raw []byte, start int, literal string) (int, bool) {
	end := start + len(literal)
	if end > len(raw) || string(raw[start:end]) != literal {
		return 0, false
	}
	return end, true
}

func skipDigits(raw []byte, position int) int {
	for position < len(raw) && isDigit(raw[position]) {
		position++
	}
	return position
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

func isHexDigit(value byte) bool {
	return isDigit(value) || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}
