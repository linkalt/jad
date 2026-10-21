package transcript

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkTurnLifecycleUniqueResponse is BenchmarkTurnLifecycle with a distinct
// response id per turn, so every commit appends a new line to messages.jsonl
// instead of hitting the duplicate check.
func BenchmarkTurnLifecycleUniqueResponse(b *testing.B) {
	for _, durability := range []DurabilityMode{DurabilityProcess, DurabilityTurn, DurabilityStrict} {
		for _, items := range []int{1, 8} {
			b.Run(fmt.Sprintf("%s/items=%d", durability, items), func(b *testing.B) {
				root := benchmarkTurnRoot(b)
				journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), durability)
				if err != nil {
					b.Fatal(err)
				}
				store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
				if err != nil {
					b.Fatal(err)
				}
				events := make([][]byte, items)
				output := ""
				for index := range events {
					item := fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}`, index)
					events[index] = outputItemEvent(int64(index), item)
					if index > 0 {
						output += ","
					}
					output += item
				}
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					turn, err := store.BeginTurn(fmt.Sprintf("turn-%d", iteration))
					if err != nil {
						b.Fatal(err)
					}
					for _, event := range events {
						if err := turn.StageOutputItem(event); err != nil {
							b.Fatal(err)
						}
					}
					completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[%s]}}`, iteration, output))
					result, err := turn.Commit(completed)
					if err != nil {
						b.Fatal(err)
					}
					if result.AlreadyPresent {
						b.Fatal("commit was a duplicate; the journal append was skipped")
					}
				}
			})
		}
	}
}
