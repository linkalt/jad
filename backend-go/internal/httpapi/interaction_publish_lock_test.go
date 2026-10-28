package httpapi

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
	"jad/backend-go/internal/tools"
)

func newInteractionContentRun(tb testing.TB, textBytes int) (*activeRun, *runContentCache) {
	tb.Helper()
	run, err := newRunRegistry(time.Minute, 1<<30, 0).begin("key", "turn")
	if err != nil {
		tb.Fatal(err)
	}
	cache := newRunContentCache(100<<10, serverconfig.DefaultRunContentWriteBatchBytes, time.Duration(serverconfig.DefaultRunContentFlushIntervalMS)*time.Millisecond)
	cache.SetPartialPath(filepath.Join(tb.TempDir(), "responses.partial"))
	run.setContentCache(cache)
	tb.Cleanup(run.disposeContent)
	if err := run.publishMessage(benchmarkMessage(1, 0, 0)); err != nil {
		tb.Fatal(err)
	}
	chunk := strings.Repeat("x", 8<<10)
	for written := 0; written < textBytes; written += len(chunk) {
		run.cacheContent(chunk)
	}
	return run, cache
}

// Reading the streamed text for an interaction publish must not hold the run lock.
func TestInteractionPublishDoesNotHoldRunLockWhileReadingContent(t *testing.T) {
	run, cache := newInteractionContentRun(t, 1<<20)
	cache.mu.Lock()
	published := make(chan struct{})
	go func() {
		run.setPendingInteraction(&tools.PendingInteraction{ID: "q1", Tool: "request_draft", ToolCallID: "call-1"}, nil)
		close(published)
	}()
	time.Sleep(50 * time.Millisecond)
	polled := make(chan struct{})
	go func() {
		_ = run.state()
		close(polled)
	}()
	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Error("run.state() blocked while an interaction publish read the content")
	}
	cache.mu.Unlock()
	<-published
}

// BenchmarkRunLockWaitDuringInteractionPublish is the wait of a run-state poll while
// another goroutine publishes interactions back to back.
func BenchmarkRunLockWaitDuringInteractionPublish(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"spilled-1MB", 1 << 20}, {"spilled-8MB", 8 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			run, _ := newInteractionContentRun(b, size.bytes)
			var stop atomic.Bool
			var group sync.WaitGroup
			group.Add(1)
			go func() {
				defer group.Done()
				for seq := uint64(1); !stop.Load(); seq++ {
					run.setPendingInteraction(&tools.PendingInteraction{ID: "q", Seq: seq, Tool: "request_draft"}, nil)
				}
			}()
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				started := time.Now()
				_ = run.state()
				waits = append(waits, time.Since(started))
				time.Sleep(time.Millisecond)
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
			b.ReportMetric(float64(waits[len(waits)/2].Microseconds()), "p50-us")
			b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
		})
	}
}
