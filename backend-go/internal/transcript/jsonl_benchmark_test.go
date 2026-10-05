package transcript

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkJournalAppend(b *testing.B) {
	journal, err := NewJournal(filepath.Join(b.TempDir(), "messages.jsonl"), DurabilityProcess)
	if err != nil {
		b.Fatal(err)
	}
	record := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"benchmark payload"}]}`)
	b.SetBytes(int64(len(record) + 1))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := journal.Append(record); err != nil {
			b.Fatal(fmt.Errorf("append iteration %d: %w", index, err))
		}
	}
}
