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
)

func newContentRun(b *testing.B, textBytes, memoryLimit int) *activeRun {
	b.Helper()
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		b.Fatal(err)
	}
	cache := newRunContentCache(memoryLimit, serverconfig.DefaultRunContentWriteBatchBytes, time.Duration(serverconfig.DefaultRunContentFlushIntervalMS)*time.Millisecond)
	cache.SetPartialPath(filepath.Join(b.TempDir(), "responses.partial"))
	run.setContentCache(cache)
	b.Cleanup(run.disposeContent)
	if err := run.publishMessage(benchmarkMessage(1, 0, 0)); err != nil {
		b.Fatal(err)
	}
	chunk := strings.Repeat("x", 8<<10)
	for written := 0; written < textBytes; written += len(chunk) {
		run.cacheContent(chunk)
	}
	return run
}

// BenchmarkLateSubscribe is one attach of a late subscriber to a running run whose
// message text lives in the content cache: subscribe builds the snapshot under the run lock.
func BenchmarkLateSubscribe(b *testing.B) {
	for _, shape := range []struct {
		name                string
		textBytes, inMemory int
	}{{"memory-64KB", 64 << 10, 1 << 20}, {"spilled-1MB", 1 << 20, 100 << 10}, {"spilled-8MB", 8 << 20, 100 << 10}} {
		b.Run(shape.name, func(b *testing.B) {
			run := newContentRun(b, shape.textBytes, shape.inMemory)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				run.detach(run.subscribe())
			}
		})
	}
}

// BenchmarkRunLockWaitDuringLateSubscribe is the wait of a run-state poll while another
// goroutine attaches late subscribers back to back; "idle" is the same poll alone.
func BenchmarkRunLockWaitDuringLateSubscribe(b *testing.B) {
	for _, shape := range []struct {
		name     string
		attached bool
	}{{"idle", false}, {"attaching-spilled-1MB", true}} {
		b.Run(shape.name, func(b *testing.B) {
			run := newContentRun(b, 1<<20, 100<<10)
			var stop atomic.Bool
			var group sync.WaitGroup
			if shape.attached {
				group.Add(1)
				go func() {
					defer group.Done()
					for !stop.Load() {
						run.detach(run.subscribe())
					}
				}()
			}
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				started := time.Now()
				_ = run.state()
				waits = append(waits, time.Since(started))
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
			b.ReportMetric(float64(waits[len(waits)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(waits[len(waits)*99/100].Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
		})
	}
}

// BenchmarkPublishLatencyDuringLateSubscribe is the wait of a publisher (one usage event)
// while another goroutine attaches late subscribers back to back; "idle" is the same
// publish alone.
func BenchmarkPublishLatencyDuringLateSubscribe(b *testing.B) {
	for _, shape := range []struct {
		name     string
		attached bool
	}{{"idle", false}, {"attaching-spilled-1MB", true}} {
		b.Run(shape.name, func(b *testing.B) {
			run := newContentRun(b, 1<<20, 100<<10)
			var stop atomic.Bool
			var group sync.WaitGroup
			if shape.attached {
				group.Add(1)
				go func() {
					defer group.Done()
					for !stop.Load() {
						run.detach(run.subscribe())
					}
				}()
			}
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				started := time.Now()
				_ = run.publish("usage", map[string]any{"usage": iteration})
				waits = append(waits, time.Since(started))
			}
			b.StopTimer()
			stop.Store(true)
			group.Wait()
			sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
			b.ReportMetric(float64(waits[len(waits)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(waits[len(waits)*99/100].Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
		})
	}
}
