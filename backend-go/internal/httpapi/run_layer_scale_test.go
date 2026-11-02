package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/session"
)

// runLayerSpawnSites are the goroutine groups this test can drive, keyed by the function
// that spawns them: the runtime prints it in the "created by" line of each stack. A
// profile run's goroutine is the one that runs executeRun, so it is the run body too.
var runLayerSpawnSites = []struct {
	name, spawner string
}{
	{"profile_driver", "profileRunStarter.StartProfileRun"},
	{"async_watcher", "(*Handler).watchAsyncState"},
}

// runLayerNotMeasured are the groups of the run layer this test cannot drive: the HTTP
// driver needs a blocking request, and the remote and queue sites live in package app,
// which imports httpapi, so this package cannot start them.
var runLayerNotMeasured = []string{"http_driver", "remote_message_watcher", "remote_subagent_start", "remote_subagent_end", "attach_waiter", "pump"}

// runLayerCount returns the goroutines per spawn site and the total of all goroutines in a
// full stack dump.
func runLayerCount(dump string) (map[string]int, int) {
	counts := map[string]int{}
	total := 0
	for _, stack := range strings.Split(dump, "\n\n") {
		if !strings.HasPrefix(stack, "goroutine ") {
			continue
		}
		total++
		for _, line := range strings.Split(stack, "\n") {
			created, ok := strings.CutPrefix(line, "created by ")
			if !ok {
				continue
			}
			spawner, _, _ := strings.Cut(created, " in goroutine")
			for _, site := range runLayerSpawnSites {
				if strings.Contains(spawner, site.spawner) {
					counts[site.name]++
				}
			}
		}
	}
	return counts, total
}

// runLayerSettleDeadline bounds how long the snapshot waits for the goroutine count to
// settle. It is a test-local constant, not product config.
const runLayerSettleDeadline = 10 * time.Second

// runLayerDump returns a full stack dump once the goroutine count stops changing. The
// buffer starts at 1 MiB and doubles only when runtime.Stack fills it, so a run count that
// needs more room does not pay for a fixed 64 MiB on every settle check.
func runLayerDump(t *testing.T, runs int) string {
	t.Helper()
	deadline := time.Now().Add(runLayerSettleDeadline)
	buf := make([]byte, 1<<20)
	previous := -1
	for {
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, 2*len(buf))
			n = runtime.Stack(buf, true)
		}
		current := strings.Count(string(buf[:n]), "\n\ngoroutine ") + 1
		if current == previous {
			return string(buf[:n])
		}
		if time.Now().After(deadline) {
			t.Fatalf("N=%d: goroutine count still changing after %s (last count %d)", runs, runLayerSettleDeadline, current)
		}
		previous = current
		time.Sleep(10 * time.Millisecond)
	}
}

// TestScaleRunLayerGoroutines counts the goroutines of N active profile runs by spawn site,
// with every run held open. Each run starts through StartProfileRun on its own session, so
// it has a profile driver (which is its run body) and an async watcher.
func TestScaleRunLayerGoroutines(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	for _, runs := range []int{100, 500, 1000} {
		release := make(chan struct{})
		resolver := &asyncResolver{fakeResolver: fakeResolver{resolved: ResolvedSession{Reference: "session", Profile: "dev", Runner: runnerFunc(func(ctx context.Context, input json.RawMessage, options session.RunOptions) (session.RunResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return session.RunResult{}, nil
		})}}}
		handler := newAsyncTestHandler(t, resolver, time.Minute)
		idle := runtime.NumGoroutine()
		runList := make([]*ProfileRun, 0, runs)
		for run := range runs {
			started, err := handler.ProfileRuns().StartProfileRun(context.Background(), ProfileRunRequest{
				Reference: fmt.Sprintf("session-%d", run), Profile: "dev", Caller: "caller", Message: "hello",
			})
			if err != nil {
				t.Fatalf("N=%d: start run %d: %v", runs, run, err)
			}
			runList = append(runList, started)
		}
		// The watchers start after StartProfileRun returns, so the snapshot waits until the
		// goroutine count stops growing: every started run has its watcher by then. The
		// deadline is a test-local constant, not product config.
		dump := runLayerDump(t, runs)
		counts, total := runLayerCount(dump)
		labels := make([]string, 0, len(runLayerSpawnSites)+len(runLayerNotMeasured))
		for _, site := range runLayerSpawnSites {
			labels = append(labels, fmt.Sprintf("%s=%d", site.name, counts[site.name]))
		}
		for _, name := range runLayerNotMeasured {
			labels = append(labels, name+"=not_measured")
		}
		t.Logf("scale-run-layer N=%d %s total=%d idle_goroutines=%d", runs, strings.Join(labels, " "), total, idle)
		close(release)
		for _, run := range runList {
			awaitProfileRunDone(t, run)
		}
	}
}
