package transcript

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkExpandTranscriptRecord measures the per-record parse, validate and
// canonicalize cost that every history read pays for each record in the file.
func BenchmarkExpandTranscriptRecord(b *testing.B) {
	text := strings.Repeat("x", 1500)
	escaped := strings.Repeat(`line \"quoted\"\n\u00e9 `, 80)
	records := map[string]string{
		"user-1.5KB":       fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}`, text),
		"completed-1.5KB":  fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, text),
		"tool-output-20KB": fmt.Sprintf(`{"type":"function_call_output","call_id":"call_1","output":"%s"}`, strings.Repeat("y", 20000)),
		"escaped-text-2KB": fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}`, escaped),
	}
	for name, record := range records {
		raw := []byte(record)
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				items, err := expandTranscriptRecord(raw)
				if err != nil || len(items) == 0 {
					b.Fatalf("expand failed: %v", err)
				}
			}
		})
	}
}
