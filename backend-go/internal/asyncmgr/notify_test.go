package asyncmgr

import "testing"

func TestOneStateChangeNotifiesWatchersOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "o", "a")
	var calls int
	cancel := f.m.Watch("o", func() { calls++ })
	defer cancel()
	f.finish(t, "a", Result{})
	if calls != 1 {
		t.Fatalf("watcher notified %d times for one completion, want 1", calls)
	}
}

func TestSnapshotLiveCarriesChangeTimesNewestFirst(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "o", "a")
	f.start(t, "o", "b")
	_, live := f.m.SnapshotLive("o")
	if len(live) != 2 || live[0].ID != "b" || !live[0].Changed.After(live[1].Changed) {
		t.Fatalf("live = %+v", live)
	}
}
