package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchmarkTurnRoot returns the directory used for turn lifecycle benchmarks.
// Set TURN_BENCH_DIR to a real-disk directory; the default is a temp dir,
// which is often tmpfs and hides the cost of metadata and fsync operations.
func benchmarkTurnRoot(b *testing.B) string {
	b.Helper()
	if base := os.Getenv("TURN_BENCH_DIR"); base != "" {
		dir, err := os.MkdirTemp(base, "turn-bench-")
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	return b.TempDir()
}

func benchmarkTurnLifecycle(b *testing.B, durability DurabilityMode, items int) {
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
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_bench","status":"completed","output":[` + output + `]}}`)
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
		if _, err := turn.Commit(completed); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTurnLifecycle(b *testing.B) {
	for _, durability := range []DurabilityMode{DurabilityProcess, DurabilityTurn, DurabilityStrict} {
		for _, items := range []int{1, 8, 32} {
			b.Run(fmt.Sprintf("%s/items=%d", durability, items), func(b *testing.B) {
				benchmarkTurnLifecycle(b, durability, items)
			})
		}
	}
}

func BenchmarkPersistManifest(b *testing.B) {
	for _, durability := range []DurabilityMode{DurabilityProcess, DurabilityTurn, DurabilityStrict} {
		b.Run(string(durability), func(b *testing.B) {
			root := benchmarkTurnRoot(b)
			journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), durability)
			if err != nil {
				b.Fatal(err)
			}
			store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
			if err != nil {
				b.Fatal(err)
			}
			turn, err := store.BeginTurn("turn-manifest")
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := turn.persistManifest(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
