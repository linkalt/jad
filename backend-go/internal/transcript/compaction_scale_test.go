package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
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

// newScaleCompactionStore opens a turn store with the same paths as the compaction benchmark.
func newScaleCompactionStore(t *testing.T, dir string) (*Journal, *TurnStore) {
	t.Helper()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "staging")
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return journal, store
}

// TestScaleCompactionStallsBeginTurn measures A6: CompactHistory runs over a 1000-turn
// transcript while N runs call BeginTurn. CompactHistory holds the store lock for its whole
// run, so every BeginTurn waits for it, and the waiting BeginTurns then queue behind one
// another. The callers start only after the lock is seen held, so compaction begins with
// no turn active. The begin latency is the time from issue until BeginTurn returns.
func TestScaleCompactionStallsBeginTurn(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	var baselineLatency float64
	for index, callers := range []int{100, 500, 1000} {
		journal, store := newScaleCompactionStore(t, t.TempDir())
		var records [][]byte
		for turn := range 1000 {
			records = append(records,
				fmt.Appendf(nil, `{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`, turn),
				fmt.Appendf(nil, `{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}}`, turn, turn))
		}
		if err := journal.Append(records...); err != nil {
			t.Fatal(err)
		}

		idle := runtime.NumGoroutine()
		start := time.Now()
		stopWatch := scaleWatchGoroutines(start)
		compacted := make(chan error, 1)
		var compactTook time.Duration
		go func() {
			_, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 10, BackupMode: "replace"})
			compactTook = time.Since(start)
			compacted <- err
		}()
		// TryLock fails only while CompactHistory holds the store lock.
		for store.mu.TryLock() {
			store.mu.Unlock()
			select {
			case err := <-compacted:
				t.Fatalf("N=%d: compaction finished before the callers started (%v)", callers, err)
			default:
			}
			time.Sleep(50 * time.Microsecond)
		}

		var (
			waitsMu sync.Mutex
			waits   []scaleSample
			group   sync.WaitGroup
		)
		for caller := range callers {
			group.Add(1)
			go func() {
				defer group.Done()
				began := time.Now()
				turn, err := store.BeginTurn(fmt.Sprintf("scale-turn-%d", caller))
				if err != nil {
					t.Error(err)
					return
				}
				waitsMu.Lock()
				waits = append(waits, scaleSample{at: time.Since(start), value: float64(time.Since(began))})
				waitsMu.Unlock()
				if err := turn.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		if err := <-compacted; err != nil {
			t.Fatal(err)
		}
		group.Wait()
		samples := stopWatch()

		p50 := scalePercentile(waits, 50)
		p99 := scalePercentile(waits, 99)
		if index == 0 {
			baselineLatency = float64(p50)
		}
		t.Logf("scale-compaction-stall N=%d begin_p50=%s begin_p99=%s compaction=%s peak_goroutines=%d idle_goroutines=%d over_2x_begin=%s",
			callers, p50, p99, compactTook.Round(time.Millisecond), scalePeakGoroutines(samples), idle,
			scaleOverText(scaleFirstAbove(waits, 2*baselineLatency), index == 0))
	}
}
