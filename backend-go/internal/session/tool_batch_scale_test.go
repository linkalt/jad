package session

import (
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scaleSample is one timed observation: at is the offset from the start of the run,
// value is a latency in nanoseconds or a goroutine count.
type scaleSample struct {
	at    time.Duration
	value float64
}

func scaleSkipUnlessMeasured(t *testing.T) {
	t.Helper()
	if os.Getenv("JAD_SCALE_MEASURE") != "1" {
		t.Skip("scale measurement: set JAD_SCALE_MEASURE=1 to run")
	}
}

// scalePercentile returns the nearest-rank percentile of the latencies in samples.
func scalePercentile(samples []scaleSample, percent int) time.Duration {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		values = append(values, sample.value)
	}
	sort.Float64s(values)
	index := (len(values)*percent+99)/100 - 1
	if index < 0 {
		return 0
	}
	return time.Duration(values[index])
}

// scaleFirstAbove returns the earliest offset of a sample above threshold, or -1 if none is.
func scaleFirstAbove(samples []scaleSample, threshold float64) time.Duration {
	first := time.Duration(-1)
	for _, sample := range samples {
		if sample.value > threshold && (first < 0 || sample.at < first) {
			first = sample.at
		}
	}
	return first
}

// scaleOverText formats a scaleFirstAbove result; the baseline row has nothing to compare with.
func scaleOverText(first time.Duration, baseline bool) string {
	switch {
	case baseline:
		return "baseline"
	case first < 0:
		return "never"
	default:
		return first.Round(time.Microsecond).String()
	}
}

// scaleWatchGoroutines samples runtime.NumGoroutine every millisecond, with offsets from
// start, until the returned function is called. The sampler itself adds one goroutine.
func scaleWatchGoroutines(start time.Time) func() []scaleSample {
	done := make(chan struct{})
	result := make(chan []scaleSample, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		var samples []scaleSample
		for {
			samples = append(samples, scaleSample{at: time.Since(start), value: float64(runtime.NumGoroutine())})
			select {
			case <-done:
				result <- samples
				return
			case <-ticker.C:
			}
		}
	}()
	return func() []scaleSample {
		close(done)
		return <-result
	}
}

func scalePeakGoroutines(samples []scaleSample) int {
	peak := 0.0
	for _, sample := range samples {
		peak = max(peak, sample.value)
	}
	return int(peak)
}

// TestScaleToolBatchFanOut measures B-01: N runs each issue one response of 64 tool calls
// at once, with the batch limit at its maximum of 64. Every call blocks until the test
// releases it, so peak_concurrent_calls is the fan-out the runs hold open at the same time.
// fanout latency is the time from issue until a run's 64th call is executing.
func TestScaleToolBatchFanOut(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	const callsPerRun = 64
	var baselineLatency, baselineGoroutines float64
	for index, runs := range []int{100, 500, 1000} {
		idle := runtime.NumGoroutine()
		start := time.Now()
		stopWatch := scaleWatchGoroutines(start)
		release := make(chan struct{})
		var active, peakActive atomic.Int64
		var fanoutMu sync.Mutex
		var fanout []scaleSample
		var group sync.WaitGroup
		for range runs {
			group.Add(1)
			go func() {
				defer group.Done()
				var started atomic.Int64
				calls := make([]int, callsPerRun)
				err := runToolBatch(calls, callsPerRun,
					func(int) (int, error) {
						current := active.Add(1)
						for {
							peak := peakActive.Load()
							if current <= peak || peakActive.CompareAndSwap(peak, current) {
								break
							}
						}
						if started.Add(1) == callsPerRun {
							elapsed := time.Since(start)
							fanoutMu.Lock()
							fanout = append(fanout, scaleSample{at: elapsed, value: float64(elapsed)})
							fanoutMu.Unlock()
						}
						<-release
						active.Add(-1)
						return 0, nil
					},
					func(int, int) error { return nil },
					nil)
				if err != nil {
					t.Error(err)
				}
			}()
		}
		want := int64(runs * callsPerRun)
		deadline := time.Now().Add(30 * time.Second)
		for active.Load() < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		saturatedAt := time.Since(start)
		close(release)
		group.Wait()
		samples := stopWatch()
		if got := peakActive.Load(); got != want {
			t.Fatalf("N=%d: %d of %d calls were executing together before the deadline", runs, got, want)
		}

		p50 := scalePercentile(fanout, 50)
		p99 := scalePercentile(fanout, 99)
		peakGoroutines := scalePeakGoroutines(samples)
		if index == 0 {
			baselineLatency = float64(p50)
			baselineGoroutines = float64(peakGoroutines)
		}
		t.Logf("scale-tool-batch N=%d fanout_p50=%s fanout_p99=%s peak_goroutines=%d idle_goroutines=%d peak_concurrent_calls=%d saturated_at=%s over_2x_latency=%s over_2x_goroutines=%s",
			runs, p50, p99, peakGoroutines, idle, peakActive.Load(), saturatedAt.Round(time.Millisecond),
			scaleOverText(scaleFirstAbove(fanout, 2*baselineLatency), index == 0),
			scaleOverText(scaleFirstAbove(samples, 2*baselineGoroutines), index == 0))
	}
}
