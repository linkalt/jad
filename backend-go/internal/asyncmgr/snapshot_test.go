package asyncmgr

import (
	"fmt"
	"sync/atomic"
	"testing"
)

func TestSnapshotListsNewestLiveIDsUpToSSEMaxIDs(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SSEMaxIDs = 3 })
	for i := 0; i < 5; i++ {
		f.start(t, "s1", fmt.Sprintf("h%d", i))
	}
	f.start(t, "s2", "other")
	counts, ids := f.m.Snapshot("s1")
	if counts.Active != 5 || counts.Unread != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if len(ids) != 3 || ids[0] != "h4" || ids[1] != "h3" || ids[2] != "h2" {
		t.Fatalf("ids = %v, want the 3 newest live ids of s1", ids)
	}
}

func TestSnapshotSkipsFinishedHandlesButCountsTheirUnreadSignal(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	f.finish(t, "a", Result{})
	counts, ids := f.m.Snapshot("s1")
	if counts.Active != 1 || counts.Unread != 1 || len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("counts = %+v ids = %v", counts, ids)
	}
	if counts, ids := f.m.Snapshot("nobody"); counts != (Counts{}) || ids != nil {
		t.Fatalf("unknown owner = %+v %v", counts, ids)
	}
}

func TestSnapshotWithZeroSSEMaxIDsSendsCountsOnly(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SSEMaxIDs = 0 })
	f.start(t, "s1", "a")
	counts, ids := f.m.Snapshot("s1")
	if counts.Active != 1 || ids != nil {
		t.Fatalf("counts = %+v ids = %v", counts, ids)
	}
}

func TestSSEMaxIDsIsBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SSEMaxIDs = MaxSSEIDs + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("sse_max_ids above the maximum was accepted")
	}
	cfg.SSEMaxIDs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative sse_max_ids was accepted")
	}
	cfg.SSEMaxIDs = MaxSSEIDs
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sse_max_ids at the maximum: %v", err)
	}
}

func TestWatchFiresForTheOwnersChangesOnlyAndStopsAfterCancel(t *testing.T) {
	f := newFixture(t, nil)
	var fired atomic.Int32
	cancel := f.m.Watch("s1", func() { fired.Add(1) })
	f.start(t, "s2", "other")
	if fired.Load() != 0 {
		t.Fatalf("a change of another owner fired the watcher %d times", fired.Load())
	}
	f.start(t, "s1", "a")
	afterStart := fired.Load()
	if afterStart == 0 {
		t.Fatal("starting a handle did not fire the watcher")
	}
	f.finish(t, "a", Result{})
	if fired.Load() <= afterStart {
		t.Fatal("completing a handle did not fire the watcher")
	}
	if _, err := f.m.Wait(background, "s1", nil, 0); err != nil {
		t.Fatal(err)
	}
	cancel()
	before := fired.Load()
	f.start(t, "s1", "b")
	if fired.Load() != before {
		t.Fatal("a cancelled watcher still fires")
	}
}
