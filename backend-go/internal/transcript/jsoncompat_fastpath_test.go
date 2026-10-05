package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// referenceStringCanonical is the pre-fast-path behaviour: decode with
// encoding/json, then re-encode with appendJSONString.
func referenceStringCanonical(raw []byte) ([]byte, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return appendJSONString(nil, value), nil
}

func fastPathStringCases() []string {
	return []string{
		`""`,
		`"plain ascii"`,
		`"with space and punctuation !#$%&'()*+,-./:;<=>?@[]^_{|}~"`,
		"\"caf\u00e9 \u4e2d\u6587 \U0001F600\"",
		"\"del\x7f char\"",
		"\"line sep \u2028 para sep \u2029\"",
		`"escaped \"quote\" and \\ backslash"`,
		`"newline \n tab \t cr \r bs \b ff \f"`,
		`"slash \/ escaped"`,
		`"unicode escape \u00e9 \u0041 \u001f"`,
		`"surrogate pair \ud83d\ude00"`,
		`"lone high surrogate \ud83d"`,
		`"lone low surrogate \ude00"`,
		"\"raw control \x01 char\"",    // invalid JSON, both paths must error
		"\"raw newline \n inside\"",    // invalid JSON
		"\"invalid utf8 \xff\xfe ok\"", // json.Unmarshal replaces with U+FFFD
		"\"truncated utf8 \xe4\xb8\"",
		"\"overlong \xc0\xaf\"",
		"\"surrogate utf8 \xed\xa0\x80\"",
		"\"nul \u0000 escaped\"",
		`"\u0000"`,
		`"`,
		`"unterminated`,
		`x"quoted"`,
		`"a"b"`,
	}
}

func TestCanonicalJSONStringFastPathMatchesReference(t *testing.T) {
	for _, source := range fastPathStringCases() {
		raw := []byte(source)
		want, wantErr := referenceStringCanonical(raw)
		got, gotErr := canonicalJSONValid(raw)
		if (wantErr == nil) != (gotErr == nil) {
			// Only inputs json.Unmarshal rejects may differ from a valid parent;
			// canonicalJSONValid is documented to receive valid JSON.
			if wantErr != nil && raw[0] == '"' {
				if !bytes.Equal(raw, []byte(`"`)) && json.Valid(raw) {
					t.Errorf("%q: reference rejected valid JSON: %v", source, wantErr)
				}
				continue
			}
			t.Errorf("%q: error mismatch: reference=%v fast=%v", source, wantErr, gotErr)
			continue
		}
		if wantErr == nil && !bytes.Equal(got, want) {
			t.Errorf("%q: canonical mismatch\n got %q\nwant %q", source, got, want)
		}
	}
}

func TestStringValueFastPathMatchesUnmarshal(t *testing.T) {
	for _, source := range fastPathStringCases() {
		raw := []byte(source)
		var want string
		wantErr := json.Unmarshal(raw, &want)
		got, ok := stringValue(raw)
		if (wantErr == nil) != ok {
			t.Errorf("%q: validity mismatch: unmarshal err=%v fast ok=%v", source, wantErr, ok)
			continue
		}
		if ok && got != want {
			t.Errorf("%q: value mismatch: got %q want %q", source, got, want)
		}
		decoded, decodeErr := jsonStringValue(raw)
		if (wantErr == nil) != (decodeErr == nil) || (decodeErr == nil && decoded != want) {
			t.Errorf("%q: jsonStringValue mismatch: got %q err=%v want %q err=%v", source, decoded, decodeErr, want, wantErr)
		}
	}
}

// referenceObjectFields is the pre-fast-path duplicate-key handling: always a map.
func referenceObjectFields(raw []byte) ([]jsonField, error) {
	position := skipJSONSpace(raw, 0)
	position++
	fields := make([]jsonField, 0)
	indices := make(map[string]int)
	for {
		position = skipJSONSpace(raw, position)
		if raw[position] == '}' {
			return fields, nil
		}
		keyEnd, err := scanJSONString(raw, position)
		if err != nil {
			return nil, err
		}
		var key string
		if err := json.Unmarshal(raw[position:keyEnd], &key); err != nil {
			return nil, err
		}
		position = skipJSONSpace(raw, keyEnd)
		valueStart := skipJSONSpace(raw, position+1)
		valueEnd, err := scanJSONValue(raw, valueStart)
		if err != nil {
			return nil, err
		}
		if index, found := indices[key]; found {
			fields[index].value = raw[valueStart:valueEnd]
		} else {
			indices[key] = len(fields)
			fields = append(fields, jsonField{key: key, value: raw[valueStart:valueEnd]})
		}
		position = skipJSONSpace(raw, valueEnd)
		if raw[position] == '}' {
			return fields, nil
		}
		position++
	}
}

func fieldsEqual(left, right []jsonField) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].key != right[index].key || !bytes.Equal(left[index].value, right[index].value) {
			return false
		}
	}
	return true
}

func TestObjectFieldsMatchesReferenceAcrossIndexThreshold(t *testing.T) {
	for _, count := range []int{0, 1, 2, 15, 16, 17, 40} {
		for _, duplicates := range []bool{false, true} {
			var builder strings.Builder
			builder.WriteByte('{')
			for index := 0; index < count; index++ {
				if index > 0 {
					builder.WriteByte(',')
				}
				fmt.Fprintf(&builder, `"k%d":%d`, index, index)
			}
			if duplicates && count > 0 {
				// Duplicates of an early key, a late key and a middle key, plus an
				// escaped spelling of the same key.
				for _, key := range []int{0, count - 1, count / 2} {
					fmt.Fprintf(&builder, `,"k%d":"dup%d"`, key, key)
				}
				fmt.Fprintf(&builder, `,"k\u0030":"escaped-dup"`)
			}
			builder.WriteByte('}')
			raw := []byte(builder.String())
			want, wantErr := referenceObjectFields(raw)
			got, gotErr := objectFields(raw)
			if wantErr != nil || gotErr != nil {
				t.Fatalf("count=%d duplicates=%v: unexpected error reference=%v fast=%v", count, duplicates, wantErr, gotErr)
			}
			if !fieldsEqual(got, want) {
				t.Errorf("count=%d duplicates=%v: fields differ\n got %v\nwant %v", count, duplicates, got, want)
			}
		}
	}
}

func FuzzCanonicalJSONMatchesReference(f *testing.F) {
	for _, source := range fastPathStringCases() {
		f.Add([]byte(source))
	}
	f.Add([]byte(`{"a":"x\ny","b":[1,2.50,"é"],"a":"dup"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '"' || !json.Valid(trimmed) {
			return
		}
		want, wantErr := referenceStringCanonical(trimmed)
		got, gotErr := canonicalJSONValid(trimmed)
		if wantErr != nil || gotErr != nil {
			t.Fatalf("%q: unexpected error reference=%v fast=%v", trimmed, wantErr, gotErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q: got %q want %q", trimmed, got, want)
		}
	})
}
