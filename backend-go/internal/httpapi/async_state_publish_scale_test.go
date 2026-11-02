package httpapi

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
)

// asyncPublishRounds is the number of changes per session in one arm.
const asyncPublishRounds = 10

// asyncPublishResolver is an AsyncStateSource over one real asyncmgr.Manager. Its
// WatchSessionAsync is a copied adapter, not the resolver method: the body of read and the
// watch setup are copied from Resolver.WatchSessionAsync (internal/app/resolver.go:1163-1197
// at ba7b6ca), line for line, with the manager list fixed to the one manager. The test
// cannot import package app, so it cannot call the real method.
type asyncPublishResolver struct {
	fakeResolver
	manager *asyncmgr.Manager
}

func (r *asyncPublishResolver) WatchSessionAsync(reference, profile string, notify func()) (func() AsyncState, func()) {
	owner := reference + "\x00" + profile
	managers := []*asyncmgr.Manager{r.manager}
	cancels := make([]func(), 0, len(managers))
	for _, manager := range managers {
		cancels = append(cancels, manager.Watch(owner, notify))
	}
	limit := int(managers[0].Config().SSEMaxIDs)
	for _, manager := range managers {
		limit = min(limit, int(manager.Config().SSEMaxIDs))
	}
	read := func() AsyncState {
		state := AsyncState{}
		var live []asyncmgr.LiveID
		for _, manager := range managers {
			counts, ids := manager.SnapshotLive(owner)
			state.Active += counts.Active
			state.Unread += counts.Unread
			live = append(live, ids...)
		}
		slices.SortFunc(live, func(a, b asyncmgr.LiveID) int {
			if c := b.Changed.Compare(a.Changed); c != 0 {
				return c
			}
			return strings.Compare(b.ID, a.ID)
		})
		for i := 0; i < len(live) && i < limit; i++ {
			state.IDs = append(state.IDs, live[i].ID)
		}
		return state
	}
	return read, func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
}

// asyncPublishDriver is the fake driver a real manager needs to accept Register.
type asyncPublishDriver struct{}

func (asyncPublishDriver) Kind() asyncmgr.Kind    { return "fake" }
func (asyncPublishDriver) Stop(id, reason string) {}

// newAsyncPublishManager opens a real manager sized for every handle the arm registers.
func newAsyncPublishManager(t *testing.T, handles int) *asyncmgr.Manager {
	t.Helper()
	cfg := asyncmgr.DefaultConfig()
	cfg.MaxHandles, cfg.MaxHandlesTotal = int64(asyncPublishRounds), int64(handles)
	cfg.KeepFinished, cfg.UnreadMax = int64(handles), int64(handles)
	m, err := asyncmgr.Open(asyncmgr.Options{Dir: t.TempDir(), Config: cfg, Server: asyncmgr.ServerID{PID: 100, Start: 1}, StopWait: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddDriver(asyncPublishDriver{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// asyncPublishSamples collects durations from concurrent callers.
type asyncPublishSamples struct {
	mu     sync.Mutex
	values []time.Duration
}

func (s *asyncPublishSamples) add(d time.Duration) {
	s.mu.Lock()
	s.values = append(s.values, d)
	s.mu.Unlock()
}

func (s *asyncPublishSamples) percentile(p int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) == 0 {
		return 0
	}
	values := append([]time.Duration(nil), s.values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[max((len(values)*p+99)/100-1, 0)]
}

// asyncPublishChange is one real Register of a session: its sequence number within the
// session (the Active count the change reaches) and the time it returned.
type asyncPublishChange struct {
	at time.Time
}

// asyncPublishTracker follows each session's published Active values. A publish that reaches
// change k (Active >= k) records the time from change k's Register to that publish.
type asyncPublishTracker struct {
	mu        sync.Mutex
	changes   [][]asyncPublishChange
	recorded  []int
	latencies asyncPublishSamples
	// drain is the time a dirty session waits for the loop: from the change to the start of its
	// publish. It is separate from publish latency, which ends at the publish.
	drain asyncPublishSamples
}

// drained records the drain wait of one publish: changedAt to the moment the loop reached it.
func (tr *asyncPublishTracker) drained(changedAt, reached time.Time) {
	tr.drain.add(reached.Sub(changedAt))
}

func newAsyncPublishTracker(sessions int) *asyncPublishTracker {
	return &asyncPublishTracker{changes: make([][]asyncPublishChange, sessions), recorded: make([]int, sessions)}
}

func (tr *asyncPublishTracker) changed(session int, at time.Time) {
	tr.mu.Lock()
	tr.changes[session] = append(tr.changes[session], asyncPublishChange{at: at})
	tr.mu.Unlock()
}

// published is called with the state a run published. It is the only place publish latency
// is recorded, so an arm that never publishes records nothing.
func (tr *asyncPublishTracker) published(session int, active int, at time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for tr.recorded[session] < min(active, len(tr.changes[session])) {
		tr.latencies.add(at.Sub(tr.changes[session][tr.recorded[session]].at))
		tr.recorded[session]++
	}
}

// asyncPublishSharedLoop is the test-only shared publisher for one manager. notify only marks
// the session dirty and does a non-blocking send to wake. The loop takes the dirty set under
// its own lock, releases it, then calls read and setAsyncState for each dirty session: each
// read is one SnapshotLive per manager, so m.mu is taken and released once per read.
//
// A session publishes at most once per interval, as watchAsyncState does (async_state.go:59
// and :74-84): a session read less than interval after its last publish stays dirty, and the
// loop waits for the rest of the interval before it looks again.
type asyncPublishSharedLoop struct {
	interval time.Duration
	mu       sync.Mutex
	dirty    map[int]time.Time
	wake     chan struct{}
	done     chan struct{}
	finished chan struct{}
	// lastPublish is touched only by the loop goroutine.
	lastPublish map[int]time.Time
}

func newAsyncPublishSharedLoop(interval time.Duration) *asyncPublishSharedLoop {
	return &asyncPublishSharedLoop{
		interval:    interval,
		dirty:       map[int]time.Time{},
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
		finished:    make(chan struct{}),
		lastPublish: map[int]time.Time{},
	}
}

// mark sets the session dirty and records when the change arrived, so the drain wait can be
// measured from the change. It runs under the manager's lock, so it never blocks.
func (p *asyncPublishSharedLoop) mark(session int) {
	now := time.Now()
	p.mu.Lock()
	if _, ok := p.dirty[session]; !ok {
		p.dirty[session] = now
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// start runs the loop. publish is called for each due session, with no loop lock held. A
// session is due when it is dirty and its last publish is at least one interval old.
func (p *asyncPublishSharedLoop) start(publish func(session int, changedAt time.Time)) {
	go func() {
		defer close(p.finished)
		for {
			select {
			case <-p.done:
				return
			case <-p.wake:
			}
			now := time.Now()
			p.mu.Lock()
			type due struct {
				session   int
				changedAt time.Time
			}
			var ready []due
			wait := time.Duration(-1)
			for session, changedAt := range p.dirty {
				next := p.lastPublish[session].Add(p.interval)
				if now.Before(next) {
					if remaining := next.Sub(now); wait < 0 || remaining < wait {
						wait = remaining
					}
					continue
				}
				ready = append(ready, due{session: session, changedAt: changedAt})
				delete(p.dirty, session)
			}
			waiting := len(p.dirty) > 0
			p.mu.Unlock()
			for _, d := range ready {
				publish(d.session, d.changedAt)
				p.lastPublish[d.session] = time.Now()
			}
			if waiting && wait >= 0 {
				// A dirty session is not due yet: sleep until it is, then look again.
				select {
				case <-p.done:
					return
				case <-time.After(wait):
				}
				select {
				case p.wake <- struct{}{}:
				default:
				}
			}
		}
	}()
}

func (p *asyncPublishSharedLoop) stop() {
	close(p.done)
	<-p.finished
}

// asyncPublishArm runs one arm at one N. loop is "per_run" (the real watchAsyncState loop),
// "shared_publisher" (the test-only loop), or "" for the loop-off baseline with no watcher.
func asyncPublishArm(t *testing.T, arm string, sessions int) {
	t.Helper()
	m := newAsyncPublishManager(t, sessions*asyncPublishRounds)
	resolver := &asyncPublishResolver{manager: m}
	handler := newAsyncTestHandler(t, resolver, 120*time.Millisecond)
	tracker := newAsyncPublishTracker(sessions)
	runs := make([]*activeRun, 0, sessions)
	references := make([]string, 0, sessions)
	for i := range sessions {
		reference := fmt.Sprintf("session-%d", i)
		run, err := handler.runs.begin(reference+"\x00dev/coder", "turn-1")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		references = append(references, reference)
	}

	var stops []func()
	var loop *asyncPublishSharedLoop
	switch arm {
	case "per_run":
		for i, run := range runs {
			stops = append(stops, handler.watchAsyncState(run, references[i], "dev/coder"))
		}
	case "shared_publisher":
		loop = newAsyncPublishSharedLoop(handler.toolStreamInterval)
		read := make([]func() AsyncState, sessions)
		for i := range sessions {
			session := i
			r, stop := resolver.WatchSessionAsync(references[i], "dev/coder", func() { loop.mark(session) })
			read[i] = r
			stops = append(stops, stop)
		}
		loop.start(func(session int, changedAt time.Time) {
			state := read[session]()
			if state.IDs == nil {
				state.IDs = []string{}
			}
			runs[session].setAsyncState(state)
			tracker.published(session, state.Active, time.Now())
			tracker.drained(changedAt, time.Now())
		})
	}
	// Residual difference: the per-run watcher publishes inside watchAsyncState, and that path
	// cannot be hooked without a production change. So the per-run arm is polled: a sampler reads
	// each run's asyncState under run.mu every 1 ms and stamps the poll time. The shared arm
	// stamps at setAsyncState (publish time). Per-run latency therefore includes up to 1 ms of
	// poll delay, and the two arms are not measured with identical clocks.
	samplerStop := make(chan struct{})
	var sampler sync.WaitGroup
	if arm == "per_run" {
		sampler.Add(1)
		go func() {
			defer sampler.Done()
			for {
				select {
				case <-samplerStop:
					return
				default:
				}
				for i, run := range runs {
					run.mu.Lock()
					active := 0
					if run.asyncState != nil {
						active = run.asyncState.Active
					}
					run.mu.Unlock()
					tracker.published(i, active, time.Now())
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Give the watchers time to start before the first change.
	time.Sleep(50 * time.Millisecond)
	var registerWait, completeWait asyncPublishSamples
	for round := range asyncPublishRounds {
		for i := range sessions {
			id := fmt.Sprintf("h-%d-%d", round, i)
			start := time.Now()
			if err := m.Register(asyncmgr.Registration{ID: id, Owner: references[i] + "\x00dev/coder", Kind: "fake", Label: "job"}); err != nil {
				t.Fatalf("N=%d: register %s: %v", sessions, id, err)
			}
			registerWait.add(time.Since(start))
			tracker.changed(i, time.Now())
		}
		for i := range sessions {
			id := fmt.Sprintf("h-%d-%d", round, i)
			start := time.Now()
			if _, err := m.Complete(id, asyncmgr.Result{State: asyncmgr.StateDone, OK: true}); err != nil {
				t.Fatalf("N=%d: complete %s: %v", sessions, id, err)
			}
			completeWait.add(time.Since(start))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if arm == "per_run" {
		close(samplerStop)
		sampler.Wait()
	}
	for _, stop := range stops {
		stop()
	}
	if loop != nil {
		loop.stop()
	}
	for _, run := range runs {
		run.finish("completed", nil)
	}
	t.Logf("scale-async-publish arm=%s N=%d publish_p50=%s publish_p99=%s publish_samples=%d register_p50=%s register_p99=%s complete_p50=%s complete_p99=%s",
		arm, sessions,
		tracker.latencies.percentile(50), tracker.latencies.percentile(99), len(tracker.latencies.values),
		registerWait.percentile(50), registerWait.percentile(99),
		completeWait.percentile(50), completeWait.percentile(99))
}

// TestScaleAsyncPublish compares per-run and shared-publisher async state publishing on the
// real asyncmgr path, against a loop-off baseline. It is gated by JAD_SCALE_MEASURE=1.
func TestScaleAsyncPublish(t *testing.T) {
	scaleSkipUnlessMeasured(t)
	for _, sessions := range []int{100, 500, 1000} {
		asyncPublishArm(t, "", sessions)
		asyncPublishArm(t, "per_run", sessions)
		asyncPublishArm(t, "shared_publisher", sessions)
	}
}
