package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/transcript"
)

// BenchmarkRepairPendingToolCalls is the run-start scan of the whole history for tool
// calls without a result, on a history that has none (the usual case). The history
// handle is created per iteration, as a run does, so the read is included.
func BenchmarkRepairPendingToolCalls(b *testing.B) {
	for _, turns := range []int{100, 1000} {
		b.Run(fmt.Sprintf("%d-turns", turns), func(b *testing.B) {
			dir := b.TempDir()
			journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
			if err != nil {
				b.Fatal(err)
			}
			store, err := transcript.NewTurnStore(journal, filepath.Join(dir, "staging"), transcript.TurnStoreOptions{MaxItemBytes: 1 << 20, ResponseMetadata: transcript.ResponseMetadataMinimal})
			if err != nil {
				b.Fatal(err)
			}
			text := strings.Repeat("x", 300)
			records := make([][]byte, 0, 4*turns)
			for turn := 0; turn < turns; turn++ {
				records = append(records,
					[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, text)),
					[]byte(fmt.Sprintf(`{"type":"function_call","call_id":"call_%d","name":"read","arguments":"{}"}`, turn)),
					[]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"ok"}`, turn)),
					[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, text)))
			}
			if err := journal.Append(records...); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				history := store.NewInputHistory()
				if err := repairPendingToolCalls(store, history); err != nil {
					b.Fatal(err)
				}
				history.Release()
			}
		})
	}
}
