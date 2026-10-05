package transcript

import "testing"

// TestExpandTranscriptRecordKeepsErrorsAndOutput pins the exact result of
// expandTranscriptRecord for valid and invalid records. The expected values were
// captured from the implementation before the validation fast paths were added.
func TestExpandTranscriptRecordKeepsErrorsAndOutput(t *testing.T) {
	cases := []struct {
		name    string
		record  string
		want    []string
		wantErr string
	}{
		{name: "user message", record: `{"role":"user","type":"message","content":[{"type":"input_text","text":"hi \u00e9\n"}]}`,
			want: []string{`{"role":"user","type":"message","content":[{"type":"input_text","text":"hi é\n"}]}`}},
		{name: "numeric keys sort first", record: `{"b":1,"2":2,"a":3,"1":4,"type":"x"}`,
			want: []string{`{"1":4,"2":2,"b":1,"a":3,"type":"x"}`}},
		{name: "duplicate keys keep first position", record: `{"type":"x","a":1,"b":2,"a":3}`,
			want: []string{`{"type":"x","a":3,"b":2}`}},
		{name: "number rendering", record: `{"type":"x","n":[1.0,1e21,1E-7,-0,0.10]}`,
			want: []string{`{"type":"x","n":[1,1e+21,1e-7,0,0.1]}`}},
		{name: "invalid utf8 replaced", record: "{\"type\":\"x\",\"s\":\"a\xffb\"}",
			want: []string{"{\"type\":\"x\",\"s\":\"a\xef\xbf\xbdb\"}"}},
		{name: "completed with two items", record: `{"type":"response.completed","response":{"output":[{"type":"message","id":"a"},{"type":"function_call","name":"n"}]}}`,
			want: []string{`{"type":"message","id":"a"}`, `{"type":"function_call","name":"n"}`}},
		{name: "completed with empty output", record: `{"type":"response.completed","response":{"output":[]}}`, want: []string{}},
		{name: "completed with padded output", record: `{"type":"response.completed","response":{"output": [ {"type":"message"} ] }}`,
			want: []string{`{"type":"message"}`}},
		{name: "response not an object", record: `{"type":"response.completed","response":[]}`, wantErr: "completed response must be an object"},
		{name: "response null", record: `{"type":"response.completed","response":null}`, wantErr: "completed response must be an object"},
		{name: "response missing", record: `{"type":"response.completed"}`, wantErr: "completed response must be an object"},
		{name: "output missing", record: `{"type":"response.completed","response":{}}`, wantErr: "committed response has no output array"},
		{name: "output is object", record: `{"type":"response.completed","response":{"output":{}}}`, wantErr: "committed response output must be an array: expected a JSON array"},
		{name: "output is null", record: `{"type":"response.completed","response":{"output":null}}`, wantErr: "committed response output must be an array: expected a JSON array"},
		{name: "output is string", record: `{"type":"response.completed","response":{"output":"x"}}`, wantErr: "committed response output must be an array: expected a JSON array"},
		{name: "output item not object", record: `{"type":"response.completed","response":{"output":[1]}}`, wantErr: "committed response output must be an array: output item must be an object"},
		{name: "output item without type", record: `{"type":"response.completed","response":{"output":[{}]}}`, wantErr: "committed response output must be an array: output item must contain a non-empty string type"},
		{name: "output item empty type", record: `{"type":"response.completed","response":{"output":[{"type":""}]}}`, wantErr: "committed response output must be an array: output item must contain a non-empty string type"},
		{name: "output item non-string type", record: `{"type":"response.completed","response":{"output":[{"type":3}]}}`, wantErr: "committed response output must be an array: output item must contain a non-empty string type"},
		{name: "bad session_turn", record: `{"type":"response.completed","session_turn":0,"response":{"output":[]}}`, wantErr: "session_turn must be a positive safe integer"},
		{name: "response event", record: `{"type":"response.created"}`, wantErr: "unsupported response event in messages.jsonl: response.created"},
		{name: "not an object", record: `[1]`, wantErr: "messages.jsonl record must be an object: expected a JSON object"},
		{name: "invalid json", record: `{"type":`, wantErr: "messages.jsonl record must be an object: expected a JSON object"},
		{name: "missing type", record: `{"a":1}`, wantErr: "input item must contain a non-empty string type"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := expandTranscriptRecord([]byte(test.record))
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("got %d items %q, want %d", len(got), got, len(test.want))
			}
			for index := range got {
				if string(got[index]) != test.want[index] {
					t.Errorf("item %d = %q, want %q", index, got[index], test.want[index])
				}
			}
		})
	}
}
