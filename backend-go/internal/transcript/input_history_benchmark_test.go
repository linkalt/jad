package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkRunRoundsHistory models one provider run of toolRounds rounds over a
// session that already holds many turns: each round reads the history and then
// the run appends one tool result. "full" re-reads the transcript every round
// (the previous behaviour); "memo" uses one InputHistory per run.
func BenchmarkRunRoundsHistory(b *testing.B) {
	const toolRounds = 10
	text := strings.Repeat("x", 1500)
	for _, turns := range []int{100, 1000, 5000} {
		for _, mode := range []string{"full", "memo"} {
			b.Run(fmt.Sprintf("%s/%d-turns", mode, turns), func(b *testing.B) {
				dir := b.TempDir()
				journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
				if err != nil {
					b.Fatal(err)
				}
				store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20, ResponseMetadata: ResponseMetadataMinimal})
				if err != nil {
					b.Fatal(err)
				}
				records := make([][]byte, 0, 2*turns)
				for turn := 0; turn < turns; turn++ {
					records = append(records,
						[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, text[:300])),
						[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, text)))
				}
				if err := journal.Append(records...); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
					base, err := journal.size()
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					history := store.NewInputHistory()
					for round := 0; round < toolRounds; round++ {
						var items [][]byte
						if mode == "memo" {
							items, err = history.Read()
						} else {
							items, err = store.ReadInputItems()
						}
						if err != nil {
							b.Fatal(err)
						}
						if len(items) < 2*turns {
							b.Fatalf("got %d items, want at least %d", len(items), 2*turns)
						}
						if err := journal.Append([]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"ok"}`, round))); err != nil {
							b.Fatal(err)
						}
					}
					history.Release()
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

func (j *Journal) size() (int64, error) {
	info, err := os.Stat(j.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func BenchmarkReadInputItems1000Records(b *testing.B) {
	journal, err := NewJournal(filepath.Join(b.TempDir(), "messages.jsonl"), DurabilityProcess)
	if err != nil {
		b.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(b.TempDir(), "staging"), TurnStoreOptions{
		MaxItemBytes:     1024 * 1024,
		ResponseMetadata: ResponseMetadataMinimal,
	})
	if err != nil {
		b.Fatal(err)
	}
	inputs := make([][]byte, 1000)
	for index := range inputs {
		inputs[index] = []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`, index))
	}
	if err := store.AppendInputItems(inputs...); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		items, err := store.ReadInputItems()
		if err != nil {
			b.Fatal(err)
		}
		if len(items) != len(inputs) {
			b.Fatalf("ReadInputItems() returned %d items, want %d", len(items), len(inputs))
		}
	}
}

// BenchmarkRunRoundsHistoryCompacted is BenchmarkRunRoundsHistory for a session
// with a compaction cache: oldTurns turns were compacted (half kept as the
// cached prefix) and 10 turns were added since.
func BenchmarkRunRoundsHistoryCompacted(b *testing.B) {
	const toolRounds = 10
	text := strings.Repeat("x", 1500)
	for _, oldTurns := range []int{1000, 5000} {
		for _, mode := range []string{"full", "memo"} {
			b.Run(fmt.Sprintf("%s/%d-compacted-turns", mode, oldTurns), func(b *testing.B) {
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
				add := func(turn int) {
					if err := store.AppendInputItems([]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, text[:300]))); err != nil {
						b.Fatal(err)
					}
					if err := journal.Append([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, text))); err != nil {
						b.Fatal(err)
					}
				}
				for turn := 0; turn < oldTurns; turn++ {
					add(turn)
				}
				if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: oldTurns / 2, BackupMode: "replace"}); err != nil {
					b.Fatal(err)
				}
				for turn := 0; turn < 10; turn++ {
					add(1_000_000 + turn)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
					base, err := journal.size()
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					history := store.NewInputHistory()
					for round := 0; round < toolRounds; round++ {
						var items [][]byte
						if mode == "memo" {
							items, err = history.Read()
						} else {
							items, err = store.ReadInputItems()
						}
						if err != nil {
							b.Fatal(err)
						}
						if len(items) < oldTurns {
							b.Fatalf("got %d items, want at least %d", len(items), oldTurns)
						}
						if err := journal.Append([]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"ok"}`, round))); err != nil {
							b.Fatal(err)
						}
					}
					history.Release()
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
