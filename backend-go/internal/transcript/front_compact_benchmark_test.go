package transcript

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkFrontCompactRecoverable1000Turns(b *testing.B) {
	dir := b.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(messages, DurabilityProcess)
	if err != nil {
		b.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		HistoryCachePath:    filepath.Join(staging, "history-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		b.Fatal(err)
	}
	records := make([][]byte, 0, 2000)
	for turn := 0; turn < 1000; turn++ {
		records = append(records,
			[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`, turn)),
			[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}}`, turn, turn)),
		)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if err := store.Reset(); err != nil {
			b.Fatal(err)
		}
		if err := journal.Append(records...); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := store.FrontCompactFromMessage("session-user-o0", "recoverable"); err != nil {
			b.Fatal(err)
		}
	}
}
