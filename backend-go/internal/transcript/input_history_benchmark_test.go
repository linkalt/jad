package transcript

import (
	"fmt"
	"path/filepath"
	"testing"
)

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
