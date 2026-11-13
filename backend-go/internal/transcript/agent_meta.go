package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// MetaField holds JAD's optional data about a record: an object of independent sub-keys,
	// written last in the record. It is a hint for JAD only: a reader treats a missing,
	// malformed or oversized value as absent (MetaValue), nothing decides ordering, turns or
	// deduplication from it, and it is stripped with every other reservedFieldPrefix key
	// before an item is sent anywhere. File order stays the truth.
	MetaField = "__agent_meta"
	// MetaStoredAt is the MetaField sub-key of a user message record: the server's UTC time,
	// RFC 3339 with milliseconds, at which JAD stored the record's input batch. It is not the
	// time the user sent it (a steering message is stored at the next round boundary).
	MetaStoredAt = "stored_at"
	// DefaultMetaMaxBytes caps the encoded MetaField value when the store is given no cap;
	// persistence.meta.max_bytes configures it.
	DefaultMetaMaxBytes = 4096

	// reservedFieldPrefix starts every top-level key that JAD writes for itself into an input
	// record. No caller may send one, and the request history never contains one.
	reservedFieldPrefix = "__agent_"
	metaStoredAtLayout  = "2006-01-02T15:04:05.000Z"
)

func isReservedField(key string) bool { return strings.HasPrefix(key, reservedFieldPrefix) }

// MetaValue returns the sub-key key of the MetaField of a messages.jsonl record. It never
// fails: a record that is not an object, a MetaField that is missing, not an object or longer
// than maxBytes (checked before the value is decoded), and a missing key all read as absent.
// A maxBytes of zero or less means DefaultMetaMaxBytes. The caller checks the value's type.
func MetaValue(record []byte, key string, maxBytes int) (json.RawMessage, bool) {
	if maxBytes <= 0 {
		maxBytes = DefaultMetaMaxBytes
	}
	fields, err := objectFields(record)
	if err != nil {
		return nil, false
	}
	raw, exists := fieldValue(fields, MetaField)
	if !exists || len(raw) > maxBytes {
		return nil, false
	}
	// objectFields validated the whole record, so the value is valid JSON.
	meta, err := objectFieldsValid(raw)
	if err != nil {
		return nil, false
	}
	value, exists := fieldValue(meta, key)
	if !exists {
		return nil, false
	}
	return append(json.RawMessage(nil), value...), true
}

// encodeMeta returns the MetaField value for a user message stored at storedAt, or nil when
// it would be longer than maxBytes: the record is then written without it.
func encodeMeta(storedAt string, maxBytes int) []byte {
	value := encodeJSONObject([]jsonField{{key: MetaStoredAt, value: appendJSONString(nil, storedAt)}})
	if len(value) > maxBytes {
		return nil
	}
	return value
}

// localTail is where the trailing JAD-only members of one record start: steer and meta are
// the offsets of the comma before SessionInputSteerField and MetaField (of the key when the
// member is the first), -1 when the record has none, and end is the offset of the closing
// brace. Blanking [steer or meta, end) with spaces leaves valid JSON of the same length.
type localTail struct {
	steer, meta, end int
}

var errNotRecord = errors.New("not a JSON object")

// locateLocalTail finds the trailing members [SessionInputSteerField:true][MetaField:<any>]
// of a record without re-encoding it. A record that is not one JSON object is errNotRecord.
// Either key anywhere else (also twice), or a steer value other than true, is an error: the
// record was not written by JAD and neither a strip nor a golden comparison may guess.
func locateLocalTail(record []byte) (localTail, error) {
	type member struct {
		key        string
		start      int
		valueStart int
		valueEnd   int
	}
	tail := localTail{steer: -1, meta: -1}
	position := skipJSONSpace(record, 0)
	if position >= len(record) || record[position] != '{' {
		return tail, errNotRecord
	}
	position++
	members := make([]member, 0, 8)
	for {
		var start int
		position = skipJSONSpace(record, position)
		if position >= len(record) {
			return tail, errNotRecord
		}
		if len(members) == 0 && record[position] == '}' {
			tail.end = position
			break
		}
		if len(members) > 0 {
			if record[position] != ',' {
				return tail, errNotRecord
			}
			start = position
			position = skipJSONSpace(record, position+1)
		} else {
			start = position
		}
		keyEnd, ok := validStringEnd(record, position)
		if !ok {
			return tail, errNotRecord
		}
		key, err := jsonStringValue(record[position:keyEnd])
		if err != nil {
			return tail, errNotRecord
		}
		position = skipJSONSpace(record, keyEnd)
		if position >= len(record) || record[position] != ':' {
			return tail, errNotRecord
		}
		valueStart := skipJSONSpace(record, position+1)
		valueEnd, ok := validFieldValueEnd(record, valueStart)
		if !ok {
			return tail, errNotRecord
		}
		members = append(members, member{key: key, start: start, valueStart: valueStart, valueEnd: valueEnd})
		position = skipJSONSpace(record, valueEnd)
		if position < len(record) && record[position] == '}' {
			tail.end = position
			break
		}
	}
	if skipJSONSpace(record, tail.end+1) != len(record) {
		return localTail{steer: -1, meta: -1}, errNotRecord
	}
	metaIndex, steerIndex, next := -1, -1, len(members)
	if next > 0 && members[next-1].key == MetaField {
		metaIndex, next = next-1, next-1
	}
	if next > 0 && members[next-1].key == SessionInputSteerField {
		steerIndex = next - 1
	}
	for index, current := range members {
		if (current.key == MetaField && index != metaIndex) || (current.key == SessionInputSteerField && index != steerIndex) {
			return localTail{steer: -1, meta: -1}, fmt.Errorf("%s is not in its place at the end of the record", current.key)
		}
	}
	if steerIndex >= 0 {
		if value := members[steerIndex]; string(record[value.valueStart:value.valueEnd]) != "true" {
			return localTail{steer: -1, meta: -1}, fmt.Errorf("%s is not true", SessionInputSteerField)
		}
		tail.steer = members[steerIndex].start
	}
	if metaIndex >= 0 {
		tail.meta = members[metaIndex].start
	}
	return tail, nil
}

// DropTrailingMeta returns line without its trailing MetaField member (the comma before it
// included), for comparisons that exclude the optional data. Everything else stays byte for
// byte. A line without the member is returned as is; a MetaField anywhere else than last, or
// a line that is not one JSON object, is an error.
func DropTrailingMeta(line []byte) ([]byte, error) {
	tail, err := locateLocalTail(line)
	if err != nil {
		return nil, err
	}
	if tail.meta < 0 {
		return line, nil
	}
	dropped := make([]byte, 0, len(line)-(tail.end-tail.meta))
	dropped = append(dropped, line[:tail.meta]...)
	return append(dropped, line[tail.end:]...), nil
}
