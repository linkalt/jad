package imageresize

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkApplyHistoryWithoutImages is the per-round cost of Apply on a long agent
// history: many function calls and their outputs, no images.
func BenchmarkApplyHistoryWithoutImages(b *testing.B) {
	for _, pairs := range []int{200, 1000, 5000} {
		b.Run(fmt.Sprintf("%d-tool-pairs", pairs), func(b *testing.B) {
			text := strings.Repeat("x", 1500)
			history := make([][]byte, 0, 2*pairs+2)
			history = append(history, []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`))
			for index := 0; index < pairs; index++ {
				history = append(history,
					[]byte(fmt.Sprintf(`{"type":"function_call","call_id":"call_%d","name":"read","arguments":"{\"path\":\"a.go\"}"}`, index)),
					[]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"%s"}`, index, text)))
			}
			resizer := New(Options{}, []string{SourceNative, SourceMCP, SourceUser})
			sourceOf := func(json.RawMessage) string { return SourceNative }
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, stats := resizer.Apply(history, sourceOf); stats.Images != 0 {
					b.Fatalf("Images = %d", stats.Images)
				}
			}
		})
	}
}
