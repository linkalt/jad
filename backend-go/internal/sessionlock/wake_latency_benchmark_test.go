package sessionlock

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// BenchmarkAcquireWakeLatencyJittered is the time from a holder's release to one waiter
// owning the lock, with the release at a random phase of the waiter's 25 ms poll. The
// fixed-sleep BenchmarkAcquireWaiters releases at a constant phase and so reports one
// point of this distribution.
func BenchmarkAcquireWakeLatencyJittered(b *testing.B) {
	path := filepath.Join(b.TempDir(), ".session.lock")
	rng := rand.New(rand.NewPCG(1, 2))
	var total, worst time.Duration
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		holder, err := Acquire(context.Background(), path)
		if err != nil {
			b.Fatal(err)
		}
		var started, done sync.WaitGroup
		started.Add(1)
		done.Add(1)
		released := make(chan time.Time, 1)
		var woke time.Duration
		go func() {
			defer done.Done()
			started.Done()
			lock, err := Acquire(context.Background(), path)
			if err != nil {
				return
			}
			woke = time.Since(<-released)
			_ = lock.Close()
		}()
		started.Wait()
		time.Sleep(acquirePollInterval + time.Duration(rng.Int64N(int64(acquirePollInterval))))
		released <- time.Now()
		_ = holder.Close()
		done.Wait()
		total += woke
		worst = max(worst, woke)
	}
	b.ReportMetric(float64(total.Microseconds())/float64(b.N), "mean-wake-us")
	b.ReportMetric(float64(worst.Microseconds()), "max-wake-us")
}
