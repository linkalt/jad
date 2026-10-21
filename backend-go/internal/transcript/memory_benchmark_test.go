package transcript

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func transcriptHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func startPeak() func() uint64 {
	done, result := make(chan struct{}), make(chan uint64)
	go func() {
		var high uint64
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > high {
				high = stats.HeapAlloc
			}
			select {
			case <-done:
				result <- high
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		return <-result
	}
}

// BenchmarkHistoryReadMemory reports, for a transcript of the given number of turns, how
// much heap one history read keeps (held-x, as a multiple of messages.jsonl) and how high
// the heap goes while it reads (peak-x). "handle" is a second read through the same
// InputHistory; "two-handles" keeps two handles alive, as two overlapping runs of one
// session would.
func BenchmarkHistoryReadMemory(b *testing.B) {
	text := strings.Repeat("x", 1500)
	for _, turns := range []int{1000, 10000} {
		for _, mode := range []string{"uncached", "handle", "two-handles"} {
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
				records := make([][]byte, 0, 1000)
				for turn := 0; turn < turns; turn++ {
					records = append(records,
						[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, text[:300])),
						[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}]}}`, turn, text)))
					if len(records) == cap(records) {
						if err := journal.Append(records...); err != nil {
							b.Fatal(err)
						}
						records = records[:0]
					}
				}
				if len(records) > 0 {
					if err := journal.Append(records...); err != nil {
						b.Fatal(err)
					}
				}
				info, err := os.Stat(journal.path)
				if err != nil {
					b.Fatal(err)
				}
				size := float64(info.Size())
				for iteration := 0; iteration < b.N; iteration++ {
					base := transcriptHeap()
					stop := startPeak()
					var keep [][][]byte
					var handles []*InputHistory
					switch mode {
					case "uncached":
						items, err := store.ReadInputItems()
						if err != nil {
							b.Fatal(err)
						}
						keep = append(keep, items)
					case "handle":
						handle := store.NewInputHistory()
						handles = append(handles, handle)
						for range 2 {
							items, err := handle.Read()
							if err != nil {
								b.Fatal(err)
							}
							keep = append(keep[:0], items)
						}
					case "two-handles":
						for range 2 {
							handle := store.NewInputHistory()
							handles = append(handles, handle)
							items, err := handle.Read()
							if err != nil {
								b.Fatal(err)
							}
							keep = append(keep, items)
						}
					}
					peak := stop()
					held := transcriptHeap()
					b.ReportMetric(size/(1<<20), "file-MiB")
					b.ReportMetric(float64(int64(peak)-int64(base))/size, "peak-x")
					b.ReportMetric(float64(int64(held)-int64(base))/size, "held-x")
					runtime.KeepAlive(keep)
					for _, handle := range handles {
						handle.Release()
					}
				}
			})
		}
	}
}

// BenchmarkLineIndexMemory is the heap the journal's duplicate-line index keeps per line of
// messages.jsonl once a commit has built it (it lives as long as the session store does).
func BenchmarkLineIndexMemory(b *testing.B) {
	for _, lines := range []int{5000, 20000} {
		b.Run(fmt.Sprintf("%d-lines", lines), func(b *testing.B) {
			journal, err := NewJournal(filepath.Join(b.TempDir(), "messages.jsonl"), DurabilityProcess)
			if err != nil {
				b.Fatal(err)
			}
			batch := make([][]byte, 0, 1000)
			for index := 0; index < lines; index++ {
				batch = append(batch, []byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`, index)))
				if len(batch) == cap(batch) {
					if err := journal.Append(batch...); err != nil {
						b.Fatal(err)
					}
					batch = batch[:0]
				}
			}
			file, err := os.Open(journal.path)
			if err != nil {
				b.Fatal(err)
			}
			defer file.Close()
			for iteration := 0; iteration < b.N; iteration++ {
				journal.mu.Lock()
				journal.index = nil
				journal.mu.Unlock()
				base := transcriptHeap()
				journal.mu.Lock()
				_, err := journal.containsLine(file, sha256.Sum256([]byte("absent")))
				indexed := len(journal.index.hashes)
				journal.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
				held := transcriptHeap()
				b.ReportMetric(float64(int64(held)-int64(base))/float64(indexed), "index-B/line")
			}
		})
	}
}
