package sessionlock

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func BenchmarkAcquireUncontended(b *testing.B) {
	path := filepath.Join(b.TempDir(), ".session.lock")
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		lock, err := Acquire(context.Background(), path)
		if err != nil {
			b.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAcquireWaiters times the drain after the holder releases: N waiters polling at
// acquirePollInterval take and release the lock in turn. ns/op is release-to-all-done.
func BenchmarkAcquireWaiters(b *testing.B) {
	for _, waiters := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("%d-waiters", waiters), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), ".session.lock")
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				holder, err := Acquire(context.Background(), path)
				if err != nil {
					b.Fatal(err)
				}
				var started, done sync.WaitGroup
				started.Add(waiters)
				done.Add(waiters)
				for index := 0; index < waiters; index++ {
					go func() {
						defer done.Done()
						started.Done()
						lock, err := Acquire(context.Background(), path)
						if err != nil {
							return
						}
						_ = lock.Close()
					}()
				}
				started.Wait()
				time.Sleep(2 * acquirePollInterval)
				b.StartTimer()
				_ = holder.Close()
				done.Wait()
				b.StopTimer()
			}
		})
	}
}
