package session

import (
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkBuildRequestWithTranscriptHistory(b *testing.B) {
	history := make([][]byte, 100)
	for index := range history {
		history[index] = []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"item-%d"}]}`, index))
	}
	settings := ModelSettings{
		Model:      "gpt-test",
		Request:    json.RawMessage(`{"temperature":0.2,"metadata":{"source":"benchmark"}}`),
		PromptRole: "developer",
		PromptText: "benchmark prompt",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := BuildRequest(settings, history); err != nil {
			b.Fatal(err)
		}
	}
}
