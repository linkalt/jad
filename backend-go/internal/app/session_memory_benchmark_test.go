package app

import (
	"context"
	"fmt"
	"runtime"
	"testing"
)

func appHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// BenchmarkResolvedSessionsRetention resolves every one of N sessions once (each opens a
// shared transcript store that the resolver keeps by storage directory) and reports the
// heap that stays per session and the goroutines added. The sessions have 20 turns each.
func BenchmarkResolvedSessionsRetention(b *testing.B) {
	for _, sessions := range []int{50, 500} {
		b.Run(fmt.Sprintf("%d-sessions", sessions), func(b *testing.B) {
			ctx := context.Background()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				resolver := newManySessionsResolver(b, sessions, 20)
				goroutines := runtime.NumGoroutine()
				base := appHeap()
				b.StartTimer()
				for index := 0; index < sessions; index++ {
					if _, err := resolver.Resolve(ctx, fmt.Sprintf("session-%04d", index), "", ""); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				held := appHeap()
				b.ReportMetric(float64(int64(held)-int64(base))/float64(sessions), "retained-B/session")
				b.ReportMetric(float64(runtime.NumGoroutine()-goroutines)/float64(sessions), "goroutines/session")
				resolver.mu.Lock()
				stores := len(resolver.stores)
				resolver.mu.Unlock()
				b.ReportMetric(float64(stores), "stores")
			}
		})
	}
}
