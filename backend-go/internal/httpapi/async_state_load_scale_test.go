package httpapi

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/serverconfig"
)

const (
	asyncLoadPerRun     = "per_run"
	asyncLoadSharedTick = "shared_tick"
	// asyncLoadInterval is the product's tool stream interval. It caps how often a run's
	// async state is published, and it is the shared tick period.
	asyncLoadInterval    = serverconfig.DefaultToolStreamIntervalMS * time.Millisecond
	asyncLoadChangeEvery = 50 * time.Millisecond
	asyncLoadWindow      = 2 * time.Second
	asyncLoadRounds      = int(asyncLoadWindow / asyncLoadChangeEvery)
)

// asyncLoadResolver is an AsyncStateSource for the load test. Like asyncmgr.Manager, it calls
// notify under its lock and reads under the same lock, so the two arms see the same contract.
// The critical section is only the fake's read, not asyncmgr's SnapshotLive, so the hold
// times it records are a lower bound for the product.
type asyncLoadResolver struct {
	fakeResolver
	loadMu  sync.Mutex
	slots   map[string]*asyncLoadSlot
	statsMu sync.Mutex
	reads   []asyncLoadRead
}

type asyncLoadSlot struct {
	active int
	notify func()
}

type asyncLoadRead struct {
	at   time.Time
	hold time.Duration
}

func (r *asyncLoadResolver) WatchSessionAsync(reference, profile string, notify func()) (func() AsyncState, func()) {
	slot := &asyncLoadSlot{notify: notify}
	r.loadMu.Lock()
	r.slots[reference] = slot
	r.loadMu.Unlock()
	read := func() AsyncState {
		r.loadMu.Lock()
		start := time.Now()
		state := AsyncState{Active: slot.active}
		hold := time.Since(start)
		r.loadMu.Unlock()
		r.statsMu.Lock()
		r.reads = append(r.reads, asyncLoadRead{at: start, hold: hold})
		r.statsMu.Unlock()
		return state
	}
	return read, func() {
		r.loadMu.Lock()
		slot.notify = nil
		r.loadMu.Unlock()
	}
}

// change sets a handle count and calls notify under the lock, as changedLocked does.
func (r *asyncLoadResolver) change(reference string, active int) {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	slot := r.slots[reference]
	slot.active = active
	if slot.notify != nil {
		slot.notify()
	}
}

func (r *asyncLoadResolver) readLog() []asyncLoadRead {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return append([]asyncLoadRead(nil), r.reads...)
}

// sharedTickPublisher is the prototype for one publisher per manager. A single goroutine
// publishes the runs whose state changed. notify only marks the run dirty and does a
// non-blocking send on wake. A tick publishes every dirty run. A wake publishes only the
// runs whose last publish is at least one interval old.
type sharedTickPublisher struct {
	interval time.Duration
	mu       sync.Mutex
	dirty    map[*sharedTickRun]struct{}
	wake     chan struct{}
	done     chan struct{}
	finished chan struct{}
}

type sharedTickRun struct {
	run  *activeRun
	read func() AsyncState
	// lastPublish is touched only by the publisher goroutine.
	lastPublish time.Time
}

func newSharedTickPublisher(interval time.Duration) *sharedTickPublisher {
	return &sharedTickPublisher{
		interval: interval,
		dirty:    map[*sharedTickRun]struct{}{},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// add follows run's session through the resolver and returns the stop function.
func (p *sharedTickPublisher) add(resolver AsyncStateSource, run *activeRun, reference, profile string) func() {
	tr := &sharedTickRun{run: run}
	read, stopWatch := resolver.WatchSessionAsync(reference, profile, func() { p.mark(tr) })
	tr.read = read
	return stopWatch
}

func (p *sharedTickPublisher) mark(tr *sharedTickRun) {
	p.mu.Lock()
	p.dirty[tr] = struct{}{}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *sharedTickPublisher) start() {
	go p.loop()
}

func (p *sharedTickPublisher) loop() {
	defer close(p.finished)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		force := false
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-ticker.C:
			force = true
		}
		p.flush(force)
	}
}

func (p *sharedTickPublisher) flush(force bool) {
	now := time.Now()
	var due []*sharedTickRun
	p.mu.Lock()
	for tr := range p.dirty {
		if force || now.Sub(tr.lastPublish) >= p.interval {
			due = append(due, tr)
			delete(p.dirty, tr)
		}
	}
	p.mu.Unlock()
	for _, tr := range due {
		state := tr.read()
		if state.IDs == nil {
			state.IDs = []string{}
		}
		tr.run.setAsyncState(state)
		tr.lastPublish = time.Now()
	}
}

func (p *sharedTickPublisher) stop() {
	close(p.done)
	<-p.finished
}

// asyncLoadSettle waits until the goroutine count stops changing, so every watcher has
// started before the changes begin. It uses the run layer's settle bound.
func asyncLoadSettle(t *testing.T, runs int) {
	t.Helper()
	deadline := time.Now().Add(runLayerSettleDeadline)
	previous := -1
	for {
		current := runtime.NumGoroutine()
		if current == previous {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("N=%d: goroutine count still changing after %s", runs, runLayerSettleDeadline)
		}
		previous = current
		time.Sleep(10 * time.Millisecond)
	}
}

// asyncLoadCollect records, for each change, the time from the change to the first
// observed published state that includes it. It polls the runs' published state.
func asyncLoadCollect(runList []*activeRun, changeAt [][]time.Time, total int, stop <-chan struct{}, collected chan<- []time.Duration) {
	recorded := make([]int, len(runList))
	var latencies []time.Duration
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(latencies) < total {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		for i, run := range runList {
			run.mu.Lock()
			active := 0
			if run.asyncState != nil {
				active = run.asyncState.Active
			}
			run.mu.Unlock()
			for ; recorded[i] < active; recorded[i]++ {
				latencies = append(latencies, now.Sub(changeAt[i][recorded[i]]))
			}
		}
	}
	collected <- latencies
}

// asyncLoadSample returns the highest goroutine count seen until stop is closed.
func asyncLoadSample(idle int, stop <-chan struct{}, peak chan<- int) {
	highest := idle
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			peak <- highest
			return
		case <-ticker.C:
			highest = max(highest, runtime.NumGoroutine())
		}
	}
}

// asyncLoadBursts returns the most reads, and the largest summed lock hold, in any one
// window of the given length counted from origin.
func asyncLoadBursts(reads []asyncLoadRead, origin time.Time, window time.Duration) (int, time.Duration) {
	counts := map[int]int{}
	holds := map[int]time.Duration{}
	mostReads := 0
	mostHold := time.Duration(0)
	for _, read := range reads {
		bucket := int(read.at.Sub(origin) / window)
		counts[bucket]++
		holds[bucket] += read.hold
		mostReads = max(mostReads, counts[bucket])
		mostHold = max(mostHold, holds[bucket])
	}
	return mostReads, mostHold
}

func asyncLoadSamples(values []time.Duration) []scaleSample {
	samples := make([]scaleSample, 0, len(values))
	for _, value := range values {
		samples = append(samples, scaleSample{value: float64(value)})
	}
	return samples
}

// runAsyncLoadArm holds N runs open, changes each one every asyncLoadChangeEvery for
// asyncLoadWindow, and measures publish latency, peak goroutines and read bursts.
func runAsyncLoadArm(t *testing.T, arm string, runs int) {
	t.Helper()
	resolver := &asyncLoadResolver{slots: map[string]*asyncLoadSlot{}}
	handler := newAsyncTestHandler(t, resolver, asyncLoadInterval)
	idle := runtime.NumGoroutine()
	var publisher *sharedTickPublisher
	if arm == asyncLoadSharedTick {
		publisher = newSharedTickPublisher(asyncLoadInterval)
	}
	runList := make([]*activeRun, 0, runs)
	references := make([]string, 0, runs)
	stops := make([]func(), 0, runs)
	for i := range runs {
		reference := fmt.Sprintf("session-%d", i)
		run, err := handler.runs.begin(reference+"\x00dev/coder", "turn-1")
		if err != nil {
			t.Fatalf("N=%d: begin run %d: %v", runs, i, err)
		}
		runList = append(runList, run)
		references = append(references, reference)
		if publisher != nil {
			stops = append(stops, publisher.add(resolver, run, reference, "dev/coder"))
		} else {
			stops = append(stops, handler.watchAsyncState(run, reference, "dev/coder"))
		}
	}
	if publisher != nil {
		publisher.start()
	}
	asyncLoadSettle(t, runs)

	changeAt := make([][]time.Time, runs)
	for i := range changeAt {
		changeAt[i] = make([]time.Time, asyncLoadRounds)
	}
	collectStop := make(chan struct{})
	defer close(collectStop)
	collected := make(chan []time.Duration, 1)
	go asyncLoadCollect(runList, changeAt, runs*asyncLoadRounds, collectStop, collected)
	samplerStop := make(chan struct{})
	peakCh := make(chan int, 1)
	go asyncLoadSample(idle, samplerStop, peakCh)

	origin := time.Now()
	for round := range asyncLoadRounds {
		for i := range runs {
			changeAt[i][round] = time.Now()
			resolver.change(references[i], round+1)
		}
		if wait := time.Until(origin.Add(time.Duration(round+1) * asyncLoadChangeEvery)); wait > 0 {
			time.Sleep(wait)
		}
	}
	var latencies []time.Duration
	select {
	case latencies = <-collected:
	case <-time.After(runLayerSettleDeadline):
		t.Fatalf("%s N=%d: changes not all published within %s", arm, runs, runLayerSettleDeadline)
	}
	drain := time.Since(origin)
	close(samplerStop)
	peak := <-peakCh

	if publisher != nil {
		publisher.stop()
	}
	for _, stop := range stops {
		stop()
	}
	var reads []asyncLoadRead
	for _, read := range resolver.readLog() {
		if !read.at.Before(origin) {
			reads = append(reads, read)
		}
	}
	for _, run := range runList {
		run.finish("completed", nil)
	}

	holds := make([]time.Duration, 0, len(reads))
	for _, read := range reads {
		holds = append(holds, read.hold)
	}
	mostReads, mostHold := asyncLoadBursts(reads, origin, asyncLoadInterval)
	latency := asyncLoadSamples(latencies)
	t.Logf("scale-async-state-load arm=%s N=%d changes=%d latency_p50=%s latency_p99=%s peak_goroutines=%d idle_goroutines=%d reads=%d largest_window_reads=%d largest_window_hold=%s read_hold_p99=%s drain=%s",
		arm, runs, len(latencies),
		scalePercentile(latency, 50), scalePercentile(latency, 99),
		peak, idle, len(reads), mostReads, mostHold,
		scalePercentile(asyncLoadSamples(holds), 99), drain)
}

// TestScaleAsyncStateLoad compares the per-run async state watcher with a shared-tick
// publisher under sustained changes. It is gated by JAD_SCALE_MEASURE=1.
func TestScaleAsyncStateLoad(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	for _, runs := range []int{100, 500, 1000} {
		for _, arm := range []string{asyncLoadPerRun, asyncLoadSharedTick} {
			runAsyncLoadArm(t, arm, runs)
		}
	}
}
