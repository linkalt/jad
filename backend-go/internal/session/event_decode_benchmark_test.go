package session

import (
	"encoding/json"
	"errors"
	"testing"
)

// BenchmarkRunnerEventDecode is the per-provider-event validation and decoding the
// runner's stream callback does before the observer sees an output_text delta
// (runner_response.go: json.Valid, decodeJSONObject, type unmarshal).
func BenchmarkRunnerEventDecode(b *testing.B) {
	raw := json.RawMessage(`{"type":"response.output_text.delta","sequence_number":42,"item_id":"msg_0123456789abcdef","output_index":0,"content_index":0,"delta":"word ","logprobs":[]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if !json.Valid(raw) {
			b.Fatal("invalid")
		}
		fields, err := decodeJSONObject(raw)
		if err != nil {
			b.Fatal(err)
		}
		typeRaw, exists := rawField(fields, "type")
		if !exists {
			b.Fatal(errors.New("no type"))
		}
		var eventType string
		if err := json.Unmarshal(typeRaw, &eventType); err != nil || eventType == "" {
			b.Fatal("bad type")
		}
	}
}
