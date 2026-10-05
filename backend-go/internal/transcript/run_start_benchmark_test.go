package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newBenchmarkHistoryStore(b *testing.B, turns int) (*Journal, *TurnStore) {
	b.Helper()
	dir := b.TempDir()
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		b.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		ResponseMetadata:    ResponseMetadataMinimal,
		MessagesBackupPath:  filepath.Join(dir, "backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
	})
	if err != nil {
		b.Fatal(err)
	}
	text := strings.Repeat("x", 1500)
	records := make([][]byte, 0, 2*turns)
	for turn := 0; turn < turns; turn++ {
		records = append(records,
			[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, text[:300])),
			[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, text)))
	}
	if err := journal.Append(records...); err != nil {
		b.Fatal(err)
	}
	return journal, store
}

// BenchmarkRunStartHistory models the history reads at the start of one run:
// the repair of pending tool calls reads the history, the user input is
// appended, and the first provider round reads the history again. "separate"
// is the previous behaviour (an uncached read, then a fresh handle); "shared"
// uses one handle for both reads.
func BenchmarkRunStartHistory(b *testing.B) {
	for _, turns := range []int{100, 1000, 5000} {
		for _, mode := range []string{"separate", "shared"} {
			b.Run(fmt.Sprintf("%s/%d-turns", mode, turns), func(b *testing.B) {
				journal, store := newBenchmarkHistoryStore(b, turns)
				input := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}`)
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
					base, err := journal.size()
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					var handle *InputHistory
					if mode == "shared" {
						handle = store.NewInputHistory()
						_, err = handle.Read()
					} else {
						_, err = store.ReadInputItems()
					}
					if err != nil {
						b.Fatal(err)
					}
					if err := store.AppendInputItems(input); err != nil {
						b.Fatal(err)
					}
					if mode != "shared" {
						handle = store.NewInputHistory()
					}
					items, err := handle.Read()
					if err != nil {
						b.Fatal(err)
					}
					if len(items) < 2*turns {
						b.Fatalf("got %d items", len(items))
					}
					handle.Release()
					b.StopTimer()
					if err := os.Truncate(journal.path, base); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		}
	}
}

// BenchmarkUncachedHistoryRead times the uncached history read that a
// compaction starts with, for comparison with the whole compaction.
func BenchmarkUncachedHistoryRead(b *testing.B) {
	for _, turns := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("%d-turns", turns), func(b *testing.B) {
			_, store := newBenchmarkHistoryStore(b, turns)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := store.ReadInputItems(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCompactHistory times one whole compaction that keeps 10 turns, for
// comparison with BenchmarkUncachedHistoryRead, the read it starts with.
func BenchmarkCompactHistory(b *testing.B) {
	for _, turns := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("%d-turns", turns), func(b *testing.B) {
			journal, store := newBenchmarkHistoryStore(b, turns)
			original, err := os.ReadFile(journal.path)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				if err := os.WriteFile(journal.path, original, 0o644); err != nil {
					b.Fatal(err)
				}
				_ = os.Remove(store.options.CompactionCachePath)
				_ = os.Remove(store.options.CompactionCachePath + ".backup")
				b.StartTimer()
				result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 10, BackupMode: "replace"})
				if err != nil || !result.Compacted {
					b.Fatalf("compact: %+v %v", result, err)
				}
			}
		})
	}
}
