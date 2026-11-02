package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
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

func scalePeakGoroutines(samples []scaleSample) int {
	peak := 0.0
	for _, sample := range samples {
		peak = max(peak, sample.value)
	}
	return int(peak)
}

// TestScaleRunAdmission measures B-03 and C-02 with N runs in N sessions and the default
// cap of 0. Begin latency is the time per admission while the registry fills. SessionRuns
// and stop are timed with all N runs held: SessionRuns scans every key, stop finds one
// session's run. The cap boundary is checked separately: a cap of N admits N runs and
// refuses the next with 429.
func TestScaleRunAdmission(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	const listCalls = 200
	var baselineBegin, baselineList float64
	for index, runs := range []int{100, 500, 1000} {
		registry := newRunRegistry(time.Minute, 1<<20, 0)
		idle := runtime.NumGoroutine()
		start := time.Now()
		var begins []scaleSample
		for run := range runs {
			began := time.Now()
			if _, err := registry.begin(fmt.Sprintf("session-%d\x00dev", run), "turn"); err != nil {
				t.Fatalf("N=%d: begin %d with default cap 0: %v", runs, run, err)
			}
			begins = append(begins, scaleSample{at: time.Since(start), value: float64(time.Since(began))})
		}
		filled := runtime.NumGoroutine()

		var lists []scaleSample
		for call := range listCalls {
			began := time.Now()
			summaries := registry.SessionRuns(fmt.Sprintf("session-%d", call%runs), "dev")
			lists = append(lists, scaleSample{at: time.Since(start), value: float64(time.Since(began))})
			if summaries.ByProfile["dev"].Status != "running" {
				t.Fatalf("N=%d: SessionRuns for session %d did not report its run as running", runs, call%runs)
			}
		}

		var stops []scaleSample
		for run := range runs {
			began := time.Now()
			result := registry.stop(fmt.Sprintf("session-%d\x00dev", run))
			stops = append(stops, scaleSample{at: time.Since(start), value: float64(time.Since(began))})
			if !result.Stopped {
				t.Fatalf("N=%d: stop of session %d was not accepted", runs, run)
			}
		}

		beginP50 := scalePercentile(begins, 50)
		listP50 := scalePercentile(lists, 50)
		if index == 0 {
			baselineBegin = float64(beginP50)
			baselineList = float64(listP50)
		}
		t.Logf("scale-run-admission N=%d begin_p50=%s begin_p99=%s session_runs_p50=%s session_runs_p99=%s stop_p50=%s peak_goroutines_filled=%d idle_goroutines=%d over_2x_begin=%s over_2x_session_runs=%s",
			runs, beginP50, scalePercentile(begins, 99), listP50, scalePercentile(lists, 99), scalePercentile(stops, 50),
			filled, idle, scaleOverText(scaleFirstAbove(begins, 2*baselineBegin), index == 0),
			scaleOverText(scaleFirstAbove(lists, 2*baselineList), index == 0))
	}
	for _, runs := range []int{100, 500, 1000} {
		registry := newRunRegistry(time.Minute, 1<<20, runs)
		for run := range runs {
			if _, err := registry.begin(fmt.Sprintf("session-%d\x00dev", run), "turn"); err != nil {
				t.Fatalf("cap=%d: begin %d within the cap: %v", runs, run, err)
			}
		}
		_, err := registry.begin("session-over\x00dev", "turn")
		statusErr, ok := err.(*StatusError)
		if !ok || statusErr.Code != http.StatusTooManyRequests {
			t.Fatalf("cap=%d: begin past the cap = %v, want 429", runs, err)
		}
		t.Logf("scale-run-cap cap=%d admitted=%d refused_past_cap=%d status=%d", runs, runs, 1, statusErr.Code)
	}
}
