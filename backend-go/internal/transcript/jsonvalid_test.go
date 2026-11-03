package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// The former readers below are kept verbatim from before the one-pass reader: objectFields
// ran json.Valid and then split with these scanners, which stepped through strings byte
// by byte. The new readers must accept the same input and split it the same way.

func formerScanJSONString(raw []byte, start int) (int, error) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, errors.New("expected JSON string")
	}
	for position := start + 1; position < len(raw); position++ {
		switch raw[position] {
		case '\\':
			position++
		case '"':
			return position + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func formerScanJSONValue(raw []byte, start int) (int, error) {
	if start >= len(raw) {
		return 0, errors.New("missing JSON value")
	}
	switch raw[start] {
	case '"':
		return formerScanJSONString(raw, start)
	case '{', '[':
		stack := make([]byte, 0, 8)
		if raw[start] == '{' {
			stack = append(stack, '}')
		} else {
			stack = append(stack, ']')
		}
		inString := false
		escaped := false
		for position := start + 1; position < len(raw); position++ {
			current := raw[position]
			if inString {
				if escaped {
					escaped = false
				} else if current == '\\' {
					escaped = true
				} else if current == '"' {
					inString = false
				}
				continue
			}
			switch current {
			case '"':
				inString = true
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

// formerObjectFields is json.Valid followed by the former split.
func formerObjectFields(raw []byte) ([]jsonField, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, errors.New("expected a JSON object")
	}
	position := skipJSONSpace(trimmed, 0) + 1
	var fields []jsonField
	for {
		position = skipJSONSpace(trimmed, position)
		if trimmed[position] == '}' {
			return fields, nil
		}
		keyEnd, err := formerScanJSONString(trimmed, position)
		if err != nil {
			return nil, err
		}
		key, err := jsonStringValue(trimmed[position:keyEnd])
		if err != nil {
			return nil, err
		}
		valueStart := skipJSONSpace(trimmed, skipJSONSpace(trimmed, keyEnd)+1)
		valueEnd, err := formerScanJSONValue(trimmed, valueStart)
		if err != nil {
			return nil, err
		}
		duplicate := false
		for index := range fields {
			if fields[index].key == key {
				fields[index].value = trimmed[valueStart:valueEnd]
				duplicate = true
			}
		}
		if !duplicate {
			fields = append(fields, jsonField{key: key, value: trimmed[valueStart:valueEnd]})
		}
		position = skipJSONSpace(trimmed, valueEnd)
		if trimmed[position] == '}' {
			return fields, nil
		}
		position++
	}
}

// jsonValidSeeds are well-formed and malformed inputs around every rule the validator
// checks; the random test mutates them further.
func jsonValidSeeds() []string {
	return []string{
		`{}`, `[]`, `""`, `0`, `-0`, `1.5e+10`, `true`, `false`, `null`,
		` {"a" : [ 1 , 2 ] } `, "\t\r\n[\n]\n",
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi \"there\" \\ \/ \b\f\n\r\t \u00e9\uD83D\uDE00"}]}`,
		`{"type":"response.completed","session_turn":3,"session_step":1,"response":{"id":"r","output":[{"type":"function_call","call_id":"c","arguments":"{\"a\":[1,{}]}"}],"usage":{"x":-1.25E-3}}}`,
		`{"a":1,"a":2,"b":{"a":{"a":[[[[]]]]}}}`,
		`{"k\u0030":1,"k0":2}`,
		"\"café \U0001F600\"", "\"raw \xff\xfe bytes\"", "\"del \x7f\"",
		// Malformed: each breaks one rule.
		``, ` `, `{`, `}`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{,"a":1}`, `{"a":1 "b":2}`, `{"a"::1}`, `{a:1}`,
		`[1,]`, `[,1]`, `[1 2]`, `[`, `]`, `[1}`, `{"a":1]`,
		`01`, `-`, `-a`, `1.`, `.5`, `1e`, `1e+`, `+1`, `1.5.5`, `0x10`, `1e5.5`, `Infinity`, `NaN`,
		`tru`, `truex`, `nul`, `nulll`, `False`,
		"\"tab \t inside\"", "\"nul \x00\"", `"bad \x escape"`, `"\u12"`, `"\u12G4"`, `"\`, `"abc`, `"a\"`,
		`{"a":1}x`, `{"a":1}{}`, `"a" "b"`, "{\"a\":1}\v", " {}",
	}
}

func mutateJSON(random *rand.Rand, source []byte) []byte {
	interesting := []byte("{}[]\":,\\ \t\n0123456789-+.eEtrufalsn\x00\x1f\x7f\xc3\xa9\xff/bu")
	result := append([]byte(nil), source...)
	for edits := 1 + random.Intn(3); edits > 0; edits-- {
		position := 0
		if len(result) > 0 {
			position = random.Intn(len(result) + 1)
		}
		switch random.Intn(3) {
		case 0:
			result = append(result[:position], append([]byte{interesting[random.Intn(len(interesting))]}, result[position:]...)...)
		case 1:
			if position < len(result) {
				result = append(result[:position], result[position+1:]...)
			}
		default:
			if position < len(result) {
				result[position] = interesting[random.Intn(len(interesting))]
			}
		}
	}
	return result
}

func checkValidMatches(t *testing.T, raw []byte) {
	t.Helper()
	if got, want := validJSON(raw), json.Valid(raw); got != want {
		t.Fatalf("validJSON(%q) = %v, json.Valid = %v", raw, got, want)
	}
	got, gotErr := objectFields(raw)
	want, wantErr := formerObjectFields(raw)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("objectFields(%q) error = %v, former = %v", raw, gotErr, wantErr)
	}
	if gotErr == nil && !fieldsEqual(got, want) {
		t.Fatalf("objectFields(%q) = %v, former = %v", raw, got, want)
	}
	if gotErr != nil && gotErr.Error() != "expected a JSON object" {
		t.Fatalf("objectFields(%q) error text = %q", raw, gotErr)
	}
}

// TestValidJSONMatchesEncodingJSON: validJSON and the one-pass objectFields accept exactly
// what json.Valid (and json.Valid followed by the former split) accepted, on the seeds and
// on 200000 random mutations of them.
func TestValidJSONMatchesEncodingJSON(t *testing.T) {
	seeds := jsonValidSeeds()
	for _, seed := range seeds {
		checkValidMatches(t, []byte(seed))
	}
	random := rand.New(rand.NewSource(11))
	for iteration := 0; iteration < 200000; iteration++ {
		checkValidMatches(t, mutateJSON(random, []byte(seeds[random.Intn(len(seeds))])))
	}
}

// TestValidJSONKeepsTheNestingLimit: encoding/json refuses more than 10000 open objects
// and arrays; so must validJSON, as an object field value too.
func TestValidJSONKeepsTheNestingLimit(t *testing.T) {
	for _, depth := range []int{maxJSONNestingDepth - 1, maxJSONNestingDepth, maxJSONNestingDepth + 1} {
		arrays := []byte(strings.Repeat("[", depth) + strings.Repeat("]", depth))
		objects := []byte(strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth))
		mixed := []byte(`{"a":` + string(arrays[:len(arrays)-2]) + `[]` + `}`)
		for _, raw := range [][]byte{arrays, objects, mixed} {
			checkValidMatches(t, raw)
		}
	}
}

// TestValidStringEndChecksEveryWordPosition puts each special byte at every offset of a
// long string, so the eight-byte check is exercised at each lane and at the tail.
func TestValidStringEndChecksEveryWordPosition(t *testing.T) {
	for _, special := range []string{`"`, `\"`, `\\`, `\n`, `\u00e9`, "\x00", "\x1f", "\x20", "\x7f", "\x80", "\xff", `\q`, `\u00`} {
		for length := 0; length < 40; length++ {
			for offset := 0; offset <= length; offset++ {
				body := strings.Repeat("a", offset) + special + strings.Repeat("b", length-offset)
				checkValidMatches(t, []byte(`"`+body+`"`))
				checkValidMatches(t, []byte(`{"k":"`+body+`"}`))
			}
		}
	}
}

// TestScanJSONStringMatchesTheFormerScanAfterBackslashRuns: the quote-to-quote jump finds
// the same end as the byte-by-byte scan for every run of backslashes before a quote.
func TestScanJSONStringMatchesTheFormerScanAfterBackslashRuns(t *testing.T) {
	random := rand.New(rand.NewSource(5))
	alphabet := []string{"a", `\\`, `\"`, `\n`, `\u0022`, "é", " "}
	for iteration := 0; iteration < 50000; iteration++ {
		var body strings.Builder
		for count := random.Intn(30); count > 0; count-- {
			body.WriteString(alphabet[random.Intn(len(alphabet))])
		}
		raw := []byte(`"` + body.String() + `"` + `,"tail\"",1]`)
		if !json.Valid(raw[:body.Len()+2]) {
			t.Fatalf("generated an invalid string %q", raw)
		}
		gotEnd, gotErr := scanJSONString(raw, 0)
		wantEnd, wantErr := formerScanJSONString(raw, 0)
		if gotEnd != wantEnd || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("scanJSONString(%q) = %d, %v; former %d, %v", raw, gotEnd, gotErr, wantEnd, wantErr)
		}
		container := []byte(`[` + string(raw[:body.Len()+2]) + `,{"k":` + string(raw[:body.Len()+2]) + `}]`)
		gotEnd, gotErr = scanJSONValue(container, 0)
		wantEnd, wantErr = formerScanJSONValue(container, 0)
		if gotEnd != wantEnd || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("scanJSONValue(%q) = %d, %v; former %d, %v", container, gotEnd, gotErr, wantEnd, wantErr)
		}
	}
}

func FuzzValidJSONMatchesEncodingJSON(f *testing.F) {
	for _, seed := range jsonValidSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkValidMatches(t, raw)
	})
}
