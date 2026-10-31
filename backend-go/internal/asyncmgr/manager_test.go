package asyncmgr

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v", err)
	}
}

func TestConfigValidateRejectsBadBounds(t *testing.T) {
	cases := map[string]func(*Config){
		"zero handles":       func(c *Config) { c.MaxHandles = 0 },
		"block order":        func(c *Config) { c.MaxBlockMs = c.DefaultBlockMs - 1 },
		"wait order":         func(c *Config) { c.MaxWaitMs = c.DefaultWaitMs - 1 },
		"list order":         func(c *Config) { c.ListMaxLimit = c.ListDefaultLimit - 1 },
		"status below entry": func(c *Config) { c.StatusSummaryBytes = c.SummaryMaxBytes - 1 },
		"budget too small":   func(c *Config) { c.ResponseMaxBytes = c.SummaryMaxBytes + EntryOverheadBytes - 1 },
		"unread below wait":  func(c *Config) { c.UnreadMax = c.WaitMaxSignals - 1 },
		"negative retention": func(c *Config) { c.RetentionDays = -1 },
	}
	for name, mutate := range cases {
		cfg := DefaultConfig()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestBlockAndWaitDurationClamp(t *testing.T) {
	cfg := DefaultConfig()
	big, neg := int64(10_000_000), int64(-5)
	if cfg.BlockDuration(nil) != 10*time.Second || cfg.BlockDuration(&big) != 120*time.Second || cfg.BlockDuration(&neg) != 0 {
		t.Fatal("BlockDuration does not default and clamp")
	}
	if cfg.WaitDuration(nil) != 10*time.Second || cfg.WaitDuration(&big) != 120*time.Second || cfg.WaitDuration(&neg) != 0 {
		t.Fatal("WaitDuration does not default and clamp")
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	if err := f.m.Register(Registration{ID: "a", Owner: "s1", Kind: "fake"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate id error = %v", err)
	}
	for _, id := range []string{"", "../x", "a/b", "index", strings.Repeat("x", 65)} {
		if err := f.m.Register(Registration{ID: id, Owner: "s1", Kind: "fake"}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("id %q error = %v, want ErrInvalidID", id, err)
		}
	}
	if err := f.m.Register(Registration{ID: "b", Owner: "s1", Kind: "nodriver"}); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("unknown kind error = %v", err)
	}
	if err := f.m.AddDriver(&fakeDriver{kind: "fake"}); err == nil {
		t.Fatal("second driver for one kind was accepted")
	}
}

func TestCapsCountLiveHandlesOnly(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxHandles, c.MaxHandlesTotal = 2, 3 })
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	err := f.m.Register(Registration{ID: "c", Owner: "s1", Kind: "fake"})
	var capacity *CapacityError
	if !errors.Is(err, ErrCapacity) || !errors.As(err, &capacity) || capacity.Scope != "owner" || capacity.Limit != 2 || capacity.Live != 2 {
		t.Fatalf("owner cap error = %v", err)
	}
	f.start(t, "s2", "d")
	if err := f.m.Register(Registration{ID: "e", Owner: "s3", Kind: "fake"}); !errors.As(err, &capacity) || capacity.Scope != "total" {
		t.Fatalf("total cap error = %v", err)
	}
	// A finished handle, even unread, frees its slot.
	f.finish(t, "a", Result{})
	f.start(t, "s1", "c")
	if got := f.m.Counts("s1"); got != (Counts{Active: 2, Unread: 1, Total: 3}) {
		t.Fatalf("Counts(s1) = %+v", got)
	}
}

func TestCompleteIsOnceAndRejectsLiveState(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	if _, err := f.m.Complete("a", Result{State: StateRunning}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("live result error = %v", err)
	}
	if _, err := f.m.Complete("nope", Result{State: StateDone}); !errors.Is(err, ErrUnknownHandle) {
		t.Fatalf("unknown handle error = %v", err)
	}
	f.finish(t, "a", Result{Summary: "first"})
	if ok, err := f.m.Complete("a", Result{State: StateFailed, Summary: "second"}); ok || err != nil {
		t.Fatalf("second Complete = %v, %v, want false, nil", ok, err)
	}
	entry, err := f.m.Status("s1", "a")
	if err != nil || entry.State != StateDone || !entry.OK || entry.Summary != "first" {
		t.Fatalf("Status = %+v, %v", entry, err)
	}
}

func TestOKIsOnlyHonouredForDone(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{State: StateFailed, OK: true})
	if entry, _ := f.m.Status("s1", "a"); entry.OK {
		t.Fatalf("failed handle reports ok: %+v", entry)
	}
}

func TestEntryFieldsAreCappedAndShaped(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SummaryMaxBytes = 16 })
	if err := f.m.Register(Registration{ID: "a", Owner: "s1", Kind: "fake", Label: strings.Repeat("é", 100)}); err != nil {
		t.Fatal(err)
	}
	f.finish(t, "a", Result{Code: "0", Ref: "/x/a", Bytes: 99, Summary: strings.Repeat("日", 20)})
	res, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(res.Signals) != 1 {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	e := res.Signals[0]
	if len(e.Label) > labelMaxBytes || !strings.HasPrefix(e.Label, "é") {
		t.Errorf("label = %q (%d bytes)", e.Label, len(e.Label))
	}
	if e.Summary != strings.Repeat("日", 5) || !e.Cut {
		t.Errorf("summary = %q cut=%v, want 5 whole runes and cut", e.Summary, e.Cut)
	}
	if e.Kind != "fake" || e.State != StateDone || !e.OK || e.Code != "0" || e.Ref != "/x/a" || e.Bytes != 99 || e.T == "" {
		t.Errorf("entry = %+v", e)
	}
}

func TestWaitNothingPendingReturnsAtOnce(t *testing.T) {
	f := newFixture(t, nil)
	start := time.Now()
	res, err := f.m.Wait(background, "s1", nil, time.Minute)
	if err != nil || len(res.Signals) != 0 || res.Pending != 0 || res.More {
		t.Fatalf("Wait = %+v, %v", res, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Wait with nothing pending blocked")
	}
}

func TestWaitTimeoutAndPoll(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	res, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(res.Signals) != 0 || res.Pending != 1 {
		t.Fatalf("poll = %+v, %v", res, err)
	}
	start := time.Now()
	res, err = f.m.Wait(background, "s1", nil, 30*time.Millisecond)
	if err != nil || len(res.Signals) != 0 || res.Pending != 1 || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("timeout = %+v, %v after %v", res, err, time.Since(start))
	}
}

func TestWaitWakesOnFirstCompletionOthersKeepRunning(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	done := make(chan WaitResult, 1)
	go func() {
		res, _ := f.m.Wait(background, "s1", nil, time.Minute)
		done <- res
	}()
	time.Sleep(20 * time.Millisecond)
	f.finish(t, "b", Result{})
	select {
	case res := <-done:
		if !reflect.DeepEqual(ids(res.Signals), []string{"b"}) || res.Pending != 1 || res.More {
			t.Fatalf("Wait = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not wake on a completion")
	}
	if got := f.m.Counts("s1"); got.Active != 1 {
		t.Fatalf("the other handle is not running: %+v", got)
	}
}

func TestWaitReturnsAllReadyOldestFirstWithMore(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.WaitMaxSignals = 3 })
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		f.start(t, "s1", id)
	}
	for _, id := range []string{"c", "a", "e", "b", "d"} {
		f.finish(t, id, Result{})
	}
	first, _ := f.m.Wait(background, "s1", nil, time.Minute)
	if !reflect.DeepEqual(ids(first.Signals), []string{"c", "a", "e"}) || !first.More {
		t.Fatalf("first wait = %+v", first)
	}
	second, _ := f.m.Wait(background, "s1", nil, time.Minute)
	if !reflect.DeepEqual(ids(second.Signals), []string{"b", "d"}) || second.More {
		t.Fatalf("second wait = %+v", second)
	}
	third, _ := f.m.Wait(background, "s1", nil, time.Minute)
	if len(third.Signals) != 0 || third.Pending != 0 {
		t.Fatalf("signals delivered twice: %+v", third)
	}
}

func TestWaitByteBudgetLeavesTheRestQueued(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.SummaryMaxBytes, c.StatusSummaryBytes = 400, 400
		c.ResponseMaxBytes = 400 + EntryOverheadBytes + 300
	})
	for _, id := range names("h", 6) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{Summary: strings.Repeat("x", 400)})
	}
	var got []string
	for range 10 {
		res, err := f.m.Wait(background, "s1", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Signals) == 0 {
			break
		}
		if size := encodedSize(res); size > int(f.m.cfg.ResponseMaxBytes) {
			t.Fatalf("response is %d bytes, budget %d", size, f.m.cfg.ResponseMaxBytes)
		}
		if len(res.Signals) >= 6 {
			t.Fatalf("budget did not limit the page: %d signals", len(res.Signals))
		}
		got = append(got, ids(res.Signals)...)
	}
	if !reflect.DeepEqual(got, names("h", 6)) {
		t.Fatalf("signals over several pages = %v, want all six in order", got)
	}
}

func TestWaitScopeIDsAndOwnerIsolation(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	f.start(t, "s2", "x")
	f.finish(t, "a", Result{})
	f.finish(t, "x", Result{})
	if _, err := f.m.Wait(background, "s1", []string{"x"}, 0); !errors.Is(err, ErrUnknownHandle) {
		t.Fatalf("waiting on another owner's handle: %v", err)
	}
	res, err := f.m.Wait(background, "s1", []string{"b"}, 0)
	if err != nil || len(res.Signals) != 0 || res.Pending != 1 {
		t.Fatalf("scoped to the running handle: %+v, %v", res, err)
	}
	res, _ = f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"a"}) {
		t.Fatalf("s1 signals = %v, want only its own", ids(res.Signals))
	}
	if _, err := f.m.Status("s1", "x"); !errors.Is(err, ErrUnknownHandle) {
		t.Fatalf("Status of another owner's handle: %v", err)
	}
	if _, err := f.m.Stop(background, "s1", "x", "r"); !errors.Is(err, ErrUnknownHandle) {
		t.Fatalf("Stop of another owner's handle: %v", err)
	}
	if len(f.driver.stopped()) != 0 {
		t.Fatal("a foreign Stop reached the driver")
	}
}

func TestWaitCancelledConsumesNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	ctx, cancel := context.WithCancel(background)
	errs := make(chan error, 1)
	go func() {
		_, err := f.m.Wait(ctx, "s1", nil, time.Minute)
		errs <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Wait error = %v", err)
	}
	f.finish(t, "a", Result{})
	if _, err := f.m.Wait(ctx, "s1", nil, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with a cancelled ctx error = %v", err)
	}
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"a"}) {
		t.Fatalf("signal consumed by a cancelled wait: %+v", res)
	}
}

func TestTwoWaitersGetEachSignalOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	results := make(chan WaitResult, 2)
	for range 2 {
		go func() {
			res, _ := f.m.Wait(background, "s1", nil, 300*time.Millisecond)
			results <- res
		}()
	}
	time.Sleep(20 * time.Millisecond)
	f.finish(t, "a", Result{})
	total := 0
	for range 2 {
		total += len((<-results).Signals)
	}
	if total != 1 {
		t.Fatalf("signal delivered %d times, want exactly once", total)
	}
}

func TestStatusDoesNotConsumeAndStopDoes(t *testing.T) {
	f := newFixture(t, nil)
	f.driver.autoComplete = true
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	f.finish(t, "a", Result{Summary: "hi"})
	if e, _ := f.m.Status("s1", "a"); e.Summary != "hi" {
		t.Fatalf("Status = %+v", e)
	}
	if got := f.m.Counts("s1"); got.Unread != 1 {
		t.Fatalf("Status consumed the signal: %+v", got)
	}
	e, err := f.m.Stop(background, "s1", "b", "model")
	if err != nil || e.State != StateStopped || e.Reason != "model" {
		t.Fatalf("Stop = %+v, %v", e, err)
	}
	again, err := f.m.Stop(background, "s1", "b", "model")
	if err != nil || again.State != StateStopped || len(f.driver.stopped()) != 1 {
		t.Fatalf("second Stop = %+v, %v, driver stops %v", again, err, f.driver.stopped())
	}
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"a"}) {
		t.Fatalf("Wait after Stop = %v, want only the signal Stop did not return", ids(res.Signals))
	}
}

func TestStopReturnsLiveEntryWhenDriverIsSlow(t *testing.T) {
	f := newFixture(t, nil)
	f.opts.StopWait = 20 * time.Millisecond
	f.m.opts.StopWait = f.opts.StopWait
	f.start(t, "s1", "a")
	e, err := f.m.Stop(background, "s1", "a", "model")
	if err != nil || e.State.Terminal() {
		t.Fatalf("Stop = %+v, %v, want the live entry", e, err)
	}
	f.finish(t, "a", Result{State: StateStopped})
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != 1 || res.Signals[0].State != StateStopped {
		t.Fatalf("late stop signal = %+v", res)
	}
}

func TestTerminalStatesAllQueueASignal(t *testing.T) {
	f := newFixture(t, nil)
	states := []State{StateDone, StateFailed, StateStopped, StateTimedOut, StateLost}
	for i, state := range states {
		id := names("t", 5)[i]
		f.start(t, "s1", id)
		f.finish(t, id, Result{State: state})
	}
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != len(states) {
		t.Fatalf("signals = %d, want %d", len(res.Signals), len(states))
	}
	for i, e := range res.Signals {
		if e.State != states[i] {
			t.Errorf("signal %d state = %s, want %s", i, e.State, states[i])
		}
	}
}

func TestQueuedHandleCountsAsLiveUntilItEnds(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.m.Register(Registration{ID: "q", Owner: "s1", Kind: "fake", Queued: true}); err != nil {
		t.Fatal(err)
	}
	if e, _ := f.m.Status("s1", "q"); e.State != StateQueued {
		t.Fatalf("state = %s", e.State)
	}
	f.m.MarkRunning("q")
	if e, _ := f.m.Status("s1", "q"); e.State != StateRunning {
		t.Fatalf("state = %s", e.State)
	}
	f.finish(t, "q", Result{})
	f.m.MarkRunning("q")
	if e, _ := f.m.Status("s1", "q"); e.State != StateDone {
		t.Fatalf("MarkRunning revived a finished handle: %s", e.State)
	}
}

type detailDriver struct{ fakeDriver }

func (d *detailDriver) Detail(id string, max int) string { return "live detail of " + id }

func TestStatusUsesDetailerForLiveHandles(t *testing.T) {
	f := newFixture(t, nil)
	f.m.mu.Lock()
	f.m.drivers["fake"] = &detailDriver{fakeDriver{kind: "fake", m: f.m}}
	f.m.mu.Unlock()
	f.start(t, "s1", "a")
	if e, _ := f.m.Status("s1", "a"); e.Summary != "live detail of a" {
		t.Fatalf("live Status = %+v", e)
	}
}

func TestStatusReadsOkUnderTheLockWhenTheRecordIsGone(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: "hi", OK: true})
	f.m.mu.Lock()
	f.m.rows["a"].rec = nil
	f.m.mu.Unlock()
	if err := os.Remove(f.m.recordPath("a")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			f.m.mu.Lock()
			f.m.rows["a"].ok = i%2 == 0
			f.m.mu.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := f.m.Status("s1", "a"); err != nil {
			t.Fatalf("Status = %v", err)
		}
	}
	<-done
}

func TestCloseWaitsForACompleteThatIsStillWritingItsRecord(t *testing.T) {
	f := newFixture(t, nil)
	f.opts.StopWait = 5 * time.Second
	f.open(t)
	f.start(t, "s1", "a")
	f.m.ioMu.Lock()
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		_, _ = f.m.Complete("a", Result{State: StateDone, OK: true})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.m.mu.Lock()
		writing := f.m.completing > 0
		f.m.mu.Unlock()
		if writing {
			break
		}
		if time.Now().After(deadline) {
			f.m.ioMu.Unlock()
			t.Fatal("Complete never started writing")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() {
		f.m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		f.m.ioMu.Unlock()
		t.Fatal("Close returned while Complete still had files to write")
	case <-time.After(150 * time.Millisecond):
	}
	f.m.ioMu.Unlock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the write finished")
	}
	<-completed
}
