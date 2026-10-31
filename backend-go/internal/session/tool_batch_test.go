package session

import (
	"bytes"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunToolBatchRunsConcurrentlyUpToLimitAndReportsEachAsItFinishes(t *testing.T) {
	var active, peak atomic.Int64
	var mu sync.Mutex
	var finished []int
	// The first wave of three calls starts together and each waits for its own release.
	// The test releases them last call first, so completion order differs from call order.
	var release [3]chan struct{}
	for i := range release {
		release[i] = make(chan struct{})
	}
	var waveStarted sync.WaitGroup
	waveStarted.Add(3)
	settled := make(chan int, 6)
	go func() {
		waveStarted.Wait()
		for _, call := range []int{2, 1, 0} {
			close(release[call])
			<-settled
		}
	}()
	err := runToolBatch([]int{0, 1, 2, 3, 4, 5}, 3,
		func(call int) (int, error) {
			now := active.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			if call < 3 {
				waveStarted.Done()
				<-release[call]
			}
			active.Add(-1)
			return call * 10, nil
		},
		func(call, output int) error {
			if output != call*10 {
				t.Errorf("output for call %d = %d", call, output)
			}
			mu.Lock()
			finished = append(finished, call)
			mu.Unlock()
			if call < 3 {
				settled <- call
			}
			return nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 3 {
		t.Fatalf("peak concurrency = %d, want 3", peak.Load())
	}
	sorted := slices.Clone(finished)
	slices.Sort(sorted)
	if !reflect.DeepEqual(sorted, []int{0, 1, 2, 3, 4, 5}) {
		t.Fatalf("finished = %v, want every call once", finished)
	}
	if slices.IsSorted(finished) {
		t.Fatalf("finished = %v, want completion order to differ from call order", finished)
	}
}

func TestRunToolBatchLimitOnePreservesSequentialOrder(t *testing.T) {
	var order []string
	err := runToolBatch([]string{"a", "b", "c"}, 1,
		func(call string) (string, error) {
			order = append(order, "start "+call)
			order = append(order, "end "+call)
			return call, nil
		},
		func(string, string) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start a", "end a", "start b", "end b", "start c", "end c"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestRunToolBatchTreatsALimitBelowOneAsSequential(t *testing.T) {
	var active, peak atomic.Int64
	err := runToolBatch([]int{0, 1, 2}, 0,
		func(int) (int, error) {
			if now := active.Add(1); now > peak.Load() {
				peak.Store(now)
			}
			active.Add(-1)
			return 0, nil
		},
		func(int, int) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency = %d, want 1", peak.Load())
	}
}

func TestRunToolBatchFailureStartsNoFurtherCallsAndLetsInFlightCallsSettle(t *testing.T) {
	var mu sync.Mutex
	var started, finished []int
	boom := errors.New("boom")
	// Call 0 fails only once call 1 is in flight, so both workers have claimed a
	// call before the failure and the outcome does not depend on scheduling.
	secondStarted := make(chan struct{})
	err := runToolBatch([]int{0, 1, 2, 3}, 2,
		func(call int) (int, error) {
			mu.Lock()
			started = append(started, call)
			mu.Unlock()
			if call == 0 {
				<-secondStarted
				return 0, boom
			}
			if call == 1 {
				close(secondStarted)
			}
			time.Sleep(40 * time.Millisecond)
			return call, nil
		},
		func(call, _ int) error {
			mu.Lock()
			finished = append(finished, call)
			mu.Unlock()
			return nil
		}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	slices.Sort(started)
	if !reflect.DeepEqual(started, []int{0, 1}) {
		t.Fatalf("started = %v, want [0 1]", started)
	}
	if !reflect.DeepEqual(finished, []int{1}) {
		t.Fatalf("finished = %v, want [1]: the in-flight call must still settle", finished)
	}
}

func TestRunToolBatchShouldStopPreventsLaterCallsFromStarting(t *testing.T) {
	var stop atomic.Bool
	var started []int
	err := runToolBatch([]int{0, 1, 2}, 1,
		func(call int) (int, error) {
			started = append(started, call)
			stop.Store(true)
			return call, nil
		},
		func(int, int) error { return nil },
		stop.Load)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(started, []int{0}) {
		t.Fatalf("started = %v, want [0]", started)
	}
}

func TestRunToolBatchReportsAFinishedHookErrorLikeAnExecutionError(t *testing.T) {
	publishFailed := errors.New("publish failed")
	err := runToolBatch([]int{0, 1}, 1,
		func(call int) (int, error) { return call, nil },
		func(int, int) error { return publishFailed }, nil)
	if !errors.Is(err, publishFailed) {
		t.Fatalf("error = %v, want publish failed", err)
	}
}

func TestRunToolBatchTurnsAPanicIntoTheCallsError(t *testing.T) {
	cases := map[string]struct {
		execute    func(int) (int, error)
		onFinished func(int, int) error
	}{
		"execute": {
			execute:    func(int) (int, error) { panic("boom in execute") },
			onFinished: func(int, int) error { return nil },
		},
		"onFinished": {
			execute:    func(call int) (int, error) { return call, nil },
			onFinished: func(int, int) error { panic("boom in onFinished") },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			started := make(chan int, 4)
			err := runToolBatch([]int{0, 1, 2, 3}, 1,
				func(call int) (int, error) {
					started <- call
					return tc.execute(call)
				},
				tc.onFinished, nil)
			// The error can reach a model, so its text is generic: no panic value, no stack.
			if err == nil || err.Error() != "tool call panicked" {
				t.Fatalf("error = %v, want the generic panic text", err)
			}
			var logged bytes.Buffer
			slog.New(slog.NewTextHandler(&logged, nil)).Error("run failed", slog.Any("error", err))
			if !strings.Contains(logged.String(), "boom in "+name) {
				t.Fatalf("log line %q has no panic value for the server log", logged.String())
			}
			if !strings.Contains(logged.String(), "goroutine") {
				t.Fatalf("log line %q has no stack for the server log", logged.String())
			}
			if len(started) != 1 {
				t.Fatalf("%d calls started, want 1: a panic fails the batch like any error", len(started))
			}
		})
	}
}

func TestRunToolBatchWithNoCallsReturnsImmediately(t *testing.T) {
	if err := runToolBatch([]int(nil), 4,
		func(int) (int, error) { t.Fatal("executed"); return 0, nil },
		func(int, int) error { t.Fatal("finished"); return nil }, nil); err != nil {
		t.Fatal(err)
	}
}

func TestToolBatchConcurrency(t *testing.T) {
	changes := func(name string) bool { return name != "read" }
	cases := []struct {
		name       string
		calls      []string
		scheduling string
		want       int
	}{
		{"ordered with a state-changing call", []string{"read", "edit"}, "ordered", 1},
		{"ordered with only a state-changing call", []string{"edit"}, "ordered", 1},
		{"ordered with read-only calls", []string{"read", "read", "read"}, "ordered", 8},
		{"ordered with no calls", nil, "ordered", 8},
		{"concurrent with a state-changing call", []string{"read", "edit"}, "concurrent", 8},
		{"empty scheduling keeps the old behaviour", []string{"edit", "edit"}, "", 8},
	}
	for _, tc := range cases {
		if got := toolBatchConcurrency(tc.calls, 8, tc.scheduling, changes); got != tc.want {
			t.Errorf("%s: concurrency = %d, want %d", tc.name, got, tc.want)
		}
	}
}
