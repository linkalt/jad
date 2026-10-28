package asyncmgr

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func slowIndex(c *Config) { c.IndexFlushMs = MaxIndexFlushMs }

func indexExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, indexFile))
	return err == nil
}

func TestIndexWriteIsDelayedAndFlushedOnClose(t *testing.T) {
	f := newFixture(t, slowIndex)
	f.start(t, "o", "a")
	f.finish(t, "a", Result{})
	if rows, _ := f.m.readIndex(); len(rows) != 0 {
		t.Fatalf("index.json has %d rows before the flush delay", len(rows))
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a.json")); err != nil {
		t.Fatalf("record not written at once: %v", err)
	}
	f.m.Close()
	rows, ok := f.m.readIndex()
	if !ok || len(rows) != 1 || rows[0].ID != "a" || rows[0].State != StateDone {
		t.Fatalf("index after Close = %+v, %v", rows, ok)
	}
}

func TestIndexBurstCostsOneDelayedWrite(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.IndexFlushMs = 30 })
	for _, id := range names("h", 20) {
		f.start(t, "o", id)
		f.finish(t, id, Result{})
	}
	poll(t, "delayed index", func() bool { return indexExists(f.dir) })
	f.m.Close()
	rows, ok := f.m.readIndex()
	if !ok || len(rows) != 20 {
		t.Fatalf("index rows = %d, %v", len(rows), ok)
	}
}

func TestRecoverFromStaleIndexKeepsLaterHandles(t *testing.T) {
	f := newFixture(t, slowIndex)
	f.start(t, "o", "old")
	f.finish(t, "old", Result{})
	f.m.Close()
	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.open(t)
	f.start(t, "o", "late")
	f.start(t, "o", "late2")
	f.finish(t, "late2", Result{})
	// crash: no Close, so the index still lists only "old".
	rows, _ := f.m.readIndex()
	if len(rows) != 1 {
		t.Fatalf("stale index rows = %d, want 1", len(rows))
	}
	f.opts.Server = ServerID{PID: 300, Start: 3}
	f.open(t)
	got := map[string]State{}
	list, err := f.m.List(ListQuery{Owner: "o", State: "all", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		got[item.ID] = item.State
	}
	if got["old"] != StateDone || got["late"] != StateLost || got["late2"] != StateDone {
		t.Fatalf("recovered states = %v", got)
	}
	if !indexExists(f.dir) {
		t.Fatal("recovery did not write the index")
	}
}

func TestPruneSkipsWhileUnderLimitsButStillExpires(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.KeepFinished = 5; c.RetentionDays = 1 })
	for _, id := range names("h", 3) {
		f.start(t, "o", id)
		f.finish(t, id, Result{})
		if _, err := f.m.Wait(background, "o", nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	f.m.mu.Lock()
	finished := f.m.finished
	f.m.mu.Unlock()
	if finished != 3 {
		t.Fatalf("finished counter = %d, want 3", finished)
	}
	f.m.Prune()
	if c := f.m.Counts("o"); c.Total != 3 {
		t.Fatalf("total after skipped prune = %d, want 3", c.Total)
	}
	f.clock.Advance(48 * time.Hour)
	f.m.Prune()
	if c := f.m.Counts("o"); c.Total != 0 {
		t.Fatalf("total after retention = %d, want 0 (skip must not hide expiry)", c.Total)
	}
	f.m.mu.Lock()
	finished = f.m.finished
	f.m.mu.Unlock()
	if finished != 0 {
		t.Fatalf("finished counter = %d, want 0", finished)
	}
}

func TestPruneSkipStillExpiresUnreadSignals(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.UnreadTTLDays = 1 })
	f.start(t, "o", "a")
	f.finish(t, "a", Result{})
	f.clock.Advance(48 * time.Hour)
	f.m.Prune()
	list, _ := f.m.List(ListQuery{Owner: "o", State: "all"})
	if len(list.Items) != 1 || list.Items[0].Unread {
		t.Fatalf("unread signal not expired: %+v", list.Items)
	}
}

func TestActiveListIsScopedToTheOwnerAndSeesLiveAndUnread(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "other", "x1")
	f.finish(t, "x1", Result{})
	for _, id := range names("r", 5) {
		f.start(t, "me", id)
		f.finish(t, id, Result{})
	}
	if _, err := f.m.Wait(background, "me", nil, 0); err != nil {
		t.Fatal(err)
	}
	f.start(t, "me", "live")
	f.start(t, "me", "fresh")
	f.finish(t, "fresh", Result{})
	list, err := f.m.List(ListQuery{Owner: "me"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range list.Items {
		got = append(got, item.ID)
	}
	if len(got) != 2 || got[0] != "fresh" || got[1] != "live" {
		t.Fatalf("active list = %v, want [fresh live]", got)
	}
	paged, _ := f.m.List(ListQuery{Owner: "me", Limit: 1})
	if len(paged.Items) != 1 || paged.Next == "" {
		t.Fatalf("page = %+v", paged)
	}
	next, _ := f.m.List(ListQuery{Owner: "me", Limit: 1, Cursor: paged.Next})
	if len(next.Items) != 1 || next.Items[0].ID != "live" {
		t.Fatalf("next page = %+v", next.Items)
	}
}

func TestRecoverAdoptsTerminalRecordOverLiveIndexRow(t *testing.T) {
	f := newFixture(t, slowIndex)
	f.start(t, "o", "a")
	f.m.flushIndexNow()
	f.finish(t, "a", Result{})
	// crash inside the flush delay: a.json is terminal, index.json still lists "a" running.
	if rows, _ := f.m.readIndex(); len(rows) != 1 || rows[0].State.Terminal() {
		t.Fatalf("stale index = %+v", rows)
	}
	f.opts.Server = ServerID{PID: 300, Start: 3}
	f.open(t)
	list, err := f.m.List(ListQuery{Owner: "o", State: "all", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].State != StateDone || !list.Items[0].Unread {
		t.Fatalf("recovered list = %+v", list.Items)
	}
	if c := f.m.Counts("o"); c.Active != 0 {
		t.Fatalf("active after recovery = %d, want 0", c.Active)
	}
	f.m.Close()
	f.open(t)
	if c := f.m.Counts("o"); c.Active != 0 {
		t.Fatalf("active after second restart = %d, want 0", c.Active)
	}
}

func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func TestCompleteAfterCloseDoesNotWriteTheFolder(t *testing.T) {
	f := newFixture(t, nil)
	f.opts.StopWait = 0
	f.m.Close()
	f.open(t)
	f.start(t, "o", "a")
	f.m.Close()
	before := dirSnapshot(t, f.dir)
	if ok, err := f.m.Complete("a", Result{State: StateDone, OK: true}); err != nil || !ok {
		t.Fatalf("Complete after Close = %v, %v", ok, err)
	}
	f.m.flushIndexNow()
	f.m.applyWork(pruneWork{removes: []string{"a"}}, true)
	after := dirSnapshot(t, f.dir)
	if len(after) != len(before) {
		t.Fatalf("files after Close: %v, before: %v", after, before)
	}
	for name, body := range before {
		if after[name] != body {
			t.Fatalf("%s changed after Close", name)
		}
	}
}

func TestPruneWithHugeDayCountsKeepsRecordsAndSignals(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.KeepFinished = 5; c.RetentionDays = 10_000_000; c.UnreadTTLDays = 10_000_000 })
	for _, id := range names("h", 3) {
		f.start(t, "o", id)
		f.finish(t, id, Result{})
	}
	f.clock.Advance(48 * time.Hour)
	f.m.Prune()
	if c := f.m.Counts("o"); c.Total != 3 || c.Unread != 3 {
		t.Fatalf("counts after prune = %+v, want 3 records and 3 unread", c)
	}
	if got := DaysDuration(10_000_000); got <= 0 {
		t.Fatalf("DaysDuration wrapped to %v", got)
	}
}
