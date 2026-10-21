package httpapi

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkSessionRunsWhilePublishing is the session-list poll (SessionRuns: registry
// lock, then each run's lock) while every one of N runs has a publisher emitting a
// 100-part, 2 KiB-per-part message back to back. 0 runs is the poll alone.
func BenchmarkSessionRunsWhilePublishing(b *testing.B) {
	for _, runs := range []int{0, 4, 16} {
		b.Run(fmt.Sprintf("%d-runs", runs), func(b *testing.B) {
			registry := newRunRegistry(time.Minute, 1<<30, 0)
			var stop atomic.Bool
			var group sync.WaitGroup
			for index := 0; index < runs; index++ {
				run, err := registry.begin(fmt.Sprintf("session-%d\x00dev", index), "turn")
				if err != nil {
					b.Fatal(err)
				}
				attachDrainers(run, 1)
				group.Add(1)
				go func() {
					defer group.Done()
					for revision := 0; !stop.Load(); revision++ {
						_ = run.publishMessage(benchmarkMessage(100, 2<<10, revision%64))
					}
				}()
			}
			waits := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				started := time.Now()
				_ = registry.SessionRuns("session-0", "dev")
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
