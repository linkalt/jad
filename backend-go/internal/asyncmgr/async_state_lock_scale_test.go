package asyncmgr

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// asyncLockSkipUnlessMeasured mirrors scaleSkipUnlessMeasured in httpapi/registry_scale_test.go.
// It is a local copy because that helper is unexported in another package.
func asyncLockSkipUnlessMeasured(t *testing.T) {
	t.Helper()
	if os.Getenv("JAD_SCALE_MEASURE") != "1" {
		t.Skip("scale measurement: set JAD_SCALE_MEASURE=1 to run")
	}
}

// asyncLockRound is the change rate and window of the measurement. Each watched owner gets
// one change every asyncLockChangeEvery for asyncLockWindow.
const (
	asyncLockChangeEvery = 50 * time.Millisecond
	asyncLockWindow      = 2 * time.Second
	asyncLockReaders     = 8
)

// asyncLockSamples collects durations from concurrent goroutines.
type asyncLockSamples struct {
	mu     sync.Mutex
	values []time.Duration
}

func (s *asyncLockSamples) add(d time.Duration) {
	s.mu.Lock()
	s.values = append(s.values, d)
	s.mu.Unlock()
}

// percentile returns the nearest-rank percentile of the samples.
func (s *asyncLockSamples) percentile(p int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) == 0 {
		return 0
	}
	values := append([]time.Duration(nil), s.values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := max((len(values)*p+99)/100-1, 0)
	return values[index]
}

// asyncLockRun measures the real manager with N watched owners. Changes come from real
// Register and Complete calls, which reach changedLocked under m.mu. Readers call the real
// SnapshotLive in a loop, as WatchSessionAsync's read does.
func asyncLockRun(t *testing.T, owners int) {
	t.Helper()
	f := newFixture(t, func(c *Config) {
		// Every round adds one live handle per owner, so the caps must hold the whole window.
		c.MaxHandles, c.MaxHandlesTotal, c.KeepFinished, c.UnreadMax = int64(asyncLockRounds()), int64(owners)*int64(asyncLockRounds()), int64(owners)*4, int64(owners)*4
	})
	f.driver.autoComplete = false

	var notifyHold asyncLockSamples
	var waitMu asyncLockSamples
	var readHold asyncLockSamples
	var readers sync.WaitGroup
	stopReaders := make(chan struct{})
	idle := runtime.NumGoroutine()

	// Watch every owner with a non-blocking notify, as watchAsyncState does. The notify runs
	// under m.mu, so its own duration is the hold time of changedLocked for that watcher.
	cancels := make([]func(), 0, owners)
	wakes := make([]chan struct{}, 0, owners)
	for i := range owners {
		wake := make(chan struct{}, 1)
		wakes = append(wakes, wake)
		cancels = append(cancels, f.m.Watch(fmt.Sprintf("owner-%d", i), func() {
			start := time.Now()
			select {
			case wake <- struct{}{}:
			default:
			}
			notifyHold.add(time.Since(start))
		}))
	}
	for range asyncLockReaders {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				for i := range owners {
					start := time.Now()
					f.m.SnapshotLive(fmt.Sprintf("owner-%d", i))
					readHold.add(time.Since(start))
				}
			}
		}()
	}

	var peak atomic.Int64
	peak.Store(int64(idle))
	samplerStop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-samplerStop:
				return
			default:
			}
			peak.Store(max(peak.Load(), int64(runtime.NumGoroutine())))
			time.Sleep(time.Millisecond)
		}
	}()

	// Each round starts one real Register per owner, which takes m.mu and notifies, and
	// times how long the caller waited for the lock.
	deadline := time.Now().Add(asyncLockWindow)
	round := 0
	for time.Now().Before(deadline) {
		for i := range owners {
			id := fmt.Sprintf("h-%d-%d", round, i)
			start := time.Now()
			if err := f.m.Register(Registration{ID: id, Owner: fmt.Sprintf("owner-%d", i), Kind: "fake", Label: "job"}); err != nil {
				t.Fatalf("N=%d: register %s: %v", owners, id, err)
			}
			waitMu.add(time.Since(start))
		}
		round++
		time.Sleep(asyncLockChangeEvery)
	}
	close(samplerStop)
	sampler.Wait()
	close(stopReaders)
	readers.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	f.m.Close()
	t.Logf("scale-async-lock N=%d rounds=%d register_calls=%d notify_hold_p50=%s notify_hold_p99=%s snapshot_live_hold_p50=%s snapshot_live_hold_p99=%s register_wait_p50=%s register_wait_p99=%s readers=%d peak_goroutines=%d idle_goroutines=%d",
		owners, round, round*owners,
		notifyHold.percentile(50), notifyHold.percentile(99),
		readHold.percentile(50), readHold.percentile(99),
		waitMu.percentile(50), waitMu.percentile(99),
		asyncLockReaders, peak.Load(), idle)
}

// TestScaleAsyncStateLockHold measures the real asyncmgr m.mu under N watched sessions:
// how long SnapshotLive and the changedLocked notify hold the lock, and how long a
// Register waits for it while readers are in flight. It is gated by JAD_SCALE_MEASURE=1.
func TestScaleAsyncStateLockHold(t *testing.T) {
	asyncLockSkipUnlessMeasured(t)
	for _, owners := range []int{100, 500, 1000} {
		asyncLockRun(t, owners)
	}
}

// asyncLockRounds is the number of change rounds in one window.
func asyncLockRounds() int {
	return int(asyncLockWindow / asyncLockChangeEvery)
}
