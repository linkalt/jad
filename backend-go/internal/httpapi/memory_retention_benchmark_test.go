package httpapi

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// peakHeapSampler records the highest HeapAlloc seen until stop is called.
func peakHeapSampler() (stop func() uint64) {
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

func finishedRun(b *testing.B, registry *runRegistry, key string, parts, bytesPerPart int) *activeRun {
	run, err := registry.begin(key, "turn")
	if err != nil {
		b.Fatal(err)
	}
	message := benchmarkMessage(parts, bytesPerPart, 0)
	if err := run.publishMessage(message); err != nil {
		b.Fatal(err)
	}
	message.Status = "complete"
	run.setCommittedMessage(message)
	terminal, err := encodeTerminalEvents(message)
	if err != nil {
		b.Fatal(err)
	}
	run.finish("completed", terminal)
	return run
}

// BenchmarkFinishedRunRetention is the heap one finished run keeps while it waits for
// its retention timer: retained-B/run is (heap with 200 retained runs - heap with none) / 200.
// "retired" repeats it with retention 0 and must return to the baseline.
func BenchmarkFinishedRunRetention(b *testing.B) {
	const runs = 200
	for _, shape := range []struct{ parts, bytes int }{{1, 2 << 10}, {1, 100 << 10}, {20, 5 << 10}} {
		for _, retention := range []time.Duration{time.Hour, 0} {
			name := fmt.Sprintf("%dx%dB/retention-%v", shape.parts, shape.bytes, retention)
			b.Run(name, func(b *testing.B) {
				for iteration := 0; iteration < b.N; iteration++ {
					registry := newRunRegistry(retention, 1<<20, 0)
					base := heapAfterGC()
					for index := 0; index < runs; index++ {
						finishedRun(b, registry, fmt.Sprintf("session-%d", index), shape.parts, shape.bytes)
					}
					held := heapAfterGC()
					b.ReportMetric(float64(int64(held)-int64(base))/runs, "retained-B/run")
					registry.close()
				}
			})
		}
	}
}

// BenchmarkRunContentCacheFallbackGrowth appends totalMB of text in 8 KiB deltas to a
// cache whose partial file cannot be created (the directory is missing), so it falls back
// to memory with no cap (A-8). "spilled" is the same stream with a working file.
// peak-x is the highest heap seen divided by the text size; snapshot-peak-x is the same
// for one Snapshot call on the finished cache.
func BenchmarkRunContentCacheFallbackGrowth(b *testing.B) {
	chunk := strings.Repeat("x", 8<<10)
	for _, mode := range []string{"fallback", "spilled"} {
		for _, totalMB := range []int{16, 64, 128} {
			b.Run(fmt.Sprintf("%s/%dMB", mode, totalMB), func(b *testing.B) {
				for iteration := 0; iteration < b.N; iteration++ {
					path := filepath.Join(b.TempDir(), "responses.partial")
					if mode == "fallback" {
						path = filepath.Join(b.TempDir(), "missing", "responses.partial")
					}
					cache := newRunContentCache(100<<10, serverconfig.DefaultRunContentWriteBatchBytes, time.Duration(serverconfig.DefaultRunContentFlushIntervalMS)*time.Millisecond)
					cache.SetPartialPath(path)
					base := heapAfterGC()
					stop := peakHeapSampler()
					for written := 0; written < totalMB<<20; written += len(chunk) {
						cache.Append(chunk)
					}
					peak := stop()
					held := heapAfterGC()
					stop = peakHeapSampler()
					text := cache.Snapshot()
					snapshotPeak := stop()
					want := totalMB << 20
					if mode == "fallback" && want > serverconfig.DefaultRunContentFallbackMaxBytes {
						want = serverconfig.DefaultRunContentFallbackMaxBytes + len(runContentTruncationMarker)
					}
					if len(text) != want {
						b.Fatalf("snapshot is %d bytes, want %d", len(text), want)
					}
					total := float64(totalMB << 20)
					b.ReportMetric(float64(int64(peak)-int64(base))/total, "peak-x")
					b.ReportMetric(float64(int64(held)-int64(base))/total, "held-x")
					b.ReportMetric(float64(int64(snapshotPeak)-int64(held))/total, "snapshot-peak-x")
					cache.Close(true)
					runtime.KeepAlive(text)
				}
			})
		}
	}
}

// BenchmarkStalledSubscriberMemory is the heap N subscribers that never read keep while a
// run publishes 400 deltas and a final snapshot: the frames are shared slices, so the cost
// per subscriber should be the queue's own slice only.
func BenchmarkStalledSubscriberMemory(b *testing.B) {
	for _, subscribers := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("%d-stalled", subscribers), func(b *testing.B) {
			for iteration := 0; iteration < b.N; iteration++ {
				registry := newRunRegistry(time.Hour, 1<<20, 0)
				run, err := registry.begin("key", "turn")
				if err != nil {
					b.Fatal(err)
				}
				queues := make([]*eventQueue, 0, subscribers)
				for index := 0; index < subscribers; index++ {
					queues = append(queues, run.subscribe())
				}
				base := heapAfterGC()
				delta := strings.Repeat("d", 2<<10)
				for index := 0; index < 400; index++ {
					if err := run.publish("ui.delta", map[string]any{"id": "m", "index": 0, "append": delta}); err != nil {
						b.Fatal(err)
					}
				}
				held := heapAfterGC()
				var queued, bytes int
				for _, queue := range queues {
					queue.mu.Lock()
					queued += len(queue.queue)
					bytes += queue.bytes
					queue.mu.Unlock()
				}
				b.ReportMetric(float64(int64(held)-int64(base))/float64(subscribers), "heap-B/subscriber")
				b.ReportMetric(float64(bytes)/float64(subscribers), "queued-B/subscriber")
				b.ReportMetric(float64(len(run.subscribers)), "still-attached")
				runtime.KeepAlive(queues)
				registry.close()
			}
		})
	}
}
