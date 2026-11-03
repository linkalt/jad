package session

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The request is written by hand so the already-encoded input is not compacted again (audit
// R1 / p2b2 PH1). Its bytes must stay exactly what json.Marshal of the field map writes.
func TestEncodeRequestFieldsMatchesJSONMarshalOfTheMap(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"a <b> & c \u2028 é"}]}`),
		json.RawMessage(`{ "type" : "function_call_output", "output" : "<html>&amp;</html>\n" }`),
	}
	input, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	cases := []map[string]json.RawMessage{
		{"model": json.RawMessage(`"m"`), "input": input, "stream": json.RawMessage("true")},
		{
			"model":         json.RawMessage(`"gpt <x>"`),
			"input":         input,
			"stream":        json.RawMessage("true"),
			"tools":         json.RawMessage(`[ {"type":"function","name":"a&b","parameters": {"type":"object"} } ]`),
			"reasoning":     json.RawMessage(`{"effort" : "high"}`),
			"<weird key>":   json.RawMessage(`null`),
			"unicode\u2028": json.RawMessage(`"\u2029"`),
			"z":             nil,
		},
	}
	for index, fields := range cases {
		want, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		got, err := encodeRequestFields(fields, "input")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("case %d:\n got %s\nwant %s", index, got, want)
		}
	}
	if _, err := encodeRequestFields(map[string]json.RawMessage{"bad": json.RawMessage(`{`)}, "input"); err == nil {
		t.Fatal("an invalid value was written")
	}
}
