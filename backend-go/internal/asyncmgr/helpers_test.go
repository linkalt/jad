package asyncmgr

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// Now advances one millisecond per call so ordering by time is deterministic.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeDriver struct {
	kind Kind
	m    *Manager
	// autoComplete makes Stop report StateStopped at once, like a driver that ends fast.
	autoComplete bool

	mu    sync.Mutex
	stops []string
	// ending (guarded by mu) is set by the end-of-test close: every stop then reports at
	// once, so that close does not wait StopWait for the handles a test left live.
	ending bool
}

func (d *fakeDriver) Kind() Kind { return d.kind }

func (d *fakeDriver) Stop(id, reason string) {
	d.mu.Lock()
	d.stops = append(d.stops, id+":"+reason)
	ending := d.ending
	d.mu.Unlock()
	if d.autoComplete || ending {
		_, _ = d.m.Complete(id, Result{State: StateStopped, Reason: reason})
	}
}

func (d *fakeDriver) stopped() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stops...)
}

type fixture struct {
	m      *Manager
	driver *fakeDriver
	clock  *fakeClock
	dir    string
	opts   Options
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	f := &fixture{clock: &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
	f.opts = Options{Dir: f.dir, Config: cfg, Server: ServerID{PID: 100, Start: 1}, Now: f.clock.Now, StopWait: 100 * time.Millisecond}
	f.open(t)
	return f
}

// open (re)opens the manager on the same directory, as a restart does. Every manager it
// opens is closed when the test ends (Close is idempotent), so no delayed index write
// lands in the folder while t.TempDir removes it.
func (f *fixture) open(t *testing.T) {
	t.Helper()
	m, err := Open(f.opts)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	driver := &fakeDriver{kind: "fake", m: m}
	t.Cleanup(func() {
		driver.mu.Lock()
		driver.ending = true
		driver.mu.Unlock()
		m.Close()
	})
	f.m = m
	f.driver = driver
	if err := m.AddDriver(driver); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) start(t *testing.T, owner, id string) {
	t.Helper()
	if err := f.m.Register(Registration{ID: id, Owner: owner, Kind: "fake", Label: "job " + id}); err != nil {
		t.Fatalf("Register(%s) error = %v", id, err)
	}
}

func (f *fixture) finish(t *testing.T, id string, res Result) {
	t.Helper()
	if res.State == "" {
		res.State = StateDone
		res.OK = true
	}
	if ok, err := f.m.Complete(id, res); err != nil || !ok {
		t.Fatalf("Complete(%s) = %v, %v", id, ok, err)
	}
}

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%04d", prefix, i)
	}
	return out
}

func poll(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

var background = context.Background()
