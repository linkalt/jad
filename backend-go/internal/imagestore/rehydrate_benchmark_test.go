package imagestore

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkRehydrateHistoryWithoutImages is the per-round cost of Rehydrate on a long
// agent history that holds no image references.
func BenchmarkRehydrateHistoryWithoutImages(b *testing.B) {
	for _, pairs := range []int{200, 1000, 5000} {
		b.Run(fmt.Sprintf("%d-tool-pairs", pairs), func(b *testing.B) {
			text := strings.Repeat("x", 1500)
			history := make([][]byte, 0, 2*pairs)
			for index := 0; index < pairs; index++ {
				history = append(history,
					[]byte(fmt.Sprintf(`{"type":"function_call","call_id":"call_%d","name":"read","arguments":"{}"}`, index)),
					[]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"%s"}`, index, text)))
			}
			store := &Store{}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, stats := store.Rehydrate(history); stats.Restored != 0 {
					b.Fatal("restored")
				}
			}
		})
	}
}
