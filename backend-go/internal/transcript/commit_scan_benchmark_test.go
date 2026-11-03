package transcript

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkCommitLargeTranscript measures one single-item response commit on a
// transcript that already holds prefillBytes of records.
func BenchmarkCommitLargeTranscript(b *testing.B) {
	for _, prefillMB := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("prefill=%dMB", prefillMB), func(b *testing.B) {
			root := benchmarkTurnRoot(b)
			journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), DurabilityProcess)
			if err != nil {
				b.Fatal(err)
			}
			filler := `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("x", 1400) + `"}]}`
			batch := make([][]byte, 0, 1024)
			for written := 0; written < prefillMB<<20; written += len(filler) + 1 {
				batch = append(batch, []byte(filler))
				if len(batch) == cap(batch) {
					if err := journal.Append(batch...); err != nil {
						b.Fatal(err)
					}
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				if err := journal.Append(batch...); err != nil {
					b.Fatal(err)
				}
			}
			store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
			if err != nil {
				b.Fatal(err)
			}
			item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				turn, err := store.BeginTurn(fmt.Sprintf("turn-%d", iteration))
				if err != nil {
					b.Fatal(err)
				}
				if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
					b.Fatal(err)
				}
				completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[%s]}}`, iteration, item))
				if _, err := turn.Commit(completed); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFirstCommitAfterOpen measures the first response commit of a freshly opened
// store (a session store built for the first time since the server started) on a
// transcript that already holds prefillMB of records.
func BenchmarkFirstCommitAfterOpen(b *testing.B) {
	for _, prefillMB := range []int{10, 50} {
		b.Run(fmt.Sprintf("prefill=%dMB", prefillMB), func(b *testing.B) {
			root := benchmarkTurnRoot(b)
			path := filepath.Join(root, "messages.jsonl")
			journal, err := NewJournal(path, DurabilityProcess)
			if err != nil {
				b.Fatal(err)
			}
			filler := `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("x", 1400) + `"}]}`
			batch := make([][]byte, 0, 1024)
			for written := 0; written < prefillMB<<20; written += len(filler) + 1 {
				batch = append(batch, []byte(filler))
				if len(batch) == cap(batch) {
					if err := journal.Append(batch...); err != nil {
						b.Fatal(err)
					}
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				if err := journal.Append(batch...); err != nil {
					b.Fatal(err)
				}
			}
			item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				journal, err := NewJournal(path, DurabilityProcess)
				if err != nil {
					b.Fatal(err)
				}
				store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
				if err != nil {
					b.Fatal(err)
				}
				turn, err := store.BeginTurn(fmt.Sprintf("turn-%d", iteration))
				if err != nil {
					b.Fatal(err)
				}
				if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
					b.Fatal(err)
				}
				completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[%s]}}`, iteration, item))
				b.StartTimer()
				if _, err := turn.Commit(completed); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
