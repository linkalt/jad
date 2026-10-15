package transcript

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkReadHistoryCompactedLargeTranscript is one tool round of a session that
// was compacted once and then grew: append a record, read the history with a
// handle that already holds the earlier rounds.
func BenchmarkReadHistoryCompactedLargeTranscript(b *testing.B) {
	for _, megabytes := range []int{10, 50, 250} {
		b.Run(fmt.Sprintf("%dMB", megabytes), func(b *testing.B) {
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
			small := strings.Repeat("x", 1500)
			var records [][]byte
			for turn := 0; turn < 200; turn++ {
				records = append(records,
					[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, small[:300])),
					[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, small)))
			}
			if err := journal.Append(records...); err != nil {
				b.Fatal(err)
			}
			if result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 100, BackupMode: "replace"}); err != nil || !result.Compacted {
				b.Fatalf("compaction: %+v %v", result, err)
			}
			big := strings.Repeat("y", 20*1024)
			batch := make([][]byte, 0, 256)
			for written := 0; written < megabytes<<20; written += 20 * 1024 {
				batch = append(batch, []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}`, big)))
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
			history := store.NewInputHistory()
			defer history.Release()
			if _, err := history.Read(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if err := journal.Append([]byte(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":"ok"}`, iteration))); err != nil {
					b.Fatal(err)
				}
				if _, err := history.Read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
