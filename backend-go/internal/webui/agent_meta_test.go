package webui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The history normaliser shows a user record with __agent_meta (well-formed or malformed)
// exactly like the record without it; showing the stored time is a later task.
func TestNormalizeTranscriptIgnoresAgentMeta(t *testing.T) {
	plain := turnRecords("o", `{"type":"response.completed","response":{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}}`,
		`{"type":"response.completed","response":{"id":"r2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]}}`)
	marked := make([]TranscriptRecord, len(plain))
	copy(marked, plain)
	for index, meta := range map[int]string{0: `,"__agent_meta":{"stored_at":"2026-11-13T07:01:02.345Z"}`, 2: `,"__agent_meta":[1]`} {
		marked[index].Raw = json.RawMessage(strings.TrimSuffix(string(plain[index].Raw), "}") + meta + "}")
	}
	want, err := NormalizeTranscript(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeTranscript(marked)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages differ:\n got %+v\nwant %+v", got, want)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "stored_at") || strings.Contains(string(encoded), "__agent_meta") {
		t.Fatalf("the meta reached the messages: %s", encoded)
	}
}
