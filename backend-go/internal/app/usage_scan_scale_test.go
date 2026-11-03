package app

import (
	"context"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
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

// TestScaleUsageScanWithAppenders measures A4/A13: a cold ProfileInfo over a transcript of
// 10k and 100k records (5k and 50k turns), while N appenders write to the same session.
// Each row uses a fresh fixture, so the appenders grow the transcript only by the appends
// made during that scan. The journal lock is held across the scan, so appends wait for it.
func TestScaleUsageScanWithAppenders(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	const appendItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"appended"}]}`
	ctx := context.Background()
	for _, records := range []int{10_000, 100_000} {
		var baselineLatency float64
		for index, appenders := range []int{100, 500, 1000} {
			resolver := newProfileTurnsBenchmarkResolver(t, 1, records/2)
			location, err := sessionfs.Resolve(resolver.roots, "demo")
			if err != nil {
				t.Fatal(err)
			}
			config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
			if err != nil {
				t.Fatal(err)
			}
			shared, _, err := resolver.sharedStore(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(config.UsageCachePath); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}

			idle := runtime.NumGoroutine()
			start := time.Now()
			stopWatch := scaleWatchGoroutines(start)
			var stop atomic.Bool
			var (
				appendMu sync.Mutex
				appends  []scaleSample
				group    sync.WaitGroup
			)
			for range appenders {
				group.Add(1)
				go func() {
					defer group.Done()
					var local []scaleSample
					for !stop.Load() {
						began := time.Now()
						if _, err := shared.store.AppendInputItemsAt([]byte(appendItem)); err != nil {
							t.Error(err)
							break
						}
						local = append(local, scaleSample{at: time.Since(start), value: float64(time.Since(began))})
					}
					appendMu.Lock()
					appends = append(appends, local...)
					appendMu.Unlock()
				}()
			}
			scanStart := time.Now()
			profile, err := profileMessenger{resolver: resolver}.ProfileInfo(ctx, "demo", "dev/p00")
			scanTook := time.Since(scanStart)
			stop.Store(true)
			group.Wait()
			samples := stopWatch()
			if err != nil {
				t.Fatal(err)
			}
			if profile == nil || *profile != 100 {
				t.Fatalf("records=%d: ProfileInfo = %v, want 100 input tokens", records, profile)
			}

			p50 := scalePercentile(appends, 50)
			p99 := scalePercentile(appends, 99)
			if index == 0 {
				baselineLatency = float64(p50)
			}
			t.Logf("scale-usage-scan records=%d N=%d scan=%s appends=%d append_p50=%s append_p99=%s peak_goroutines=%d idle_goroutines=%d over_2x_append=%s",
				records, appenders, scanTook.Round(time.Millisecond), len(appends), p50, p99,
				scalePeakGoroutines(samples), idle, scaleOverText(scaleFirstAbove(appends, 2*baselineLatency), index == 0))
		}
	}
}
