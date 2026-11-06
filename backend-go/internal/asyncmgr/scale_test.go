package asyncmgr

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestListDefaultsToActiveNewestFirstWithCounts(t *testing.T) {
	f := newFixture(t, nil)
	for _, id := range []string{"a", "b", "c", "d"} {
		f.start(t, "s1", id)
	}
	f.finish(t, "a", Result{})
	f.finish(t, "b", Result{})
	if _, err := f.m.Wait(background, "s1", []string{"a"}, 0); err != nil {
		t.Fatal(err)
	}
	res, err := f.m.List(ListQuery{Owner: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range res.Items {
		got = append(got, item.ID)
	}
	// a is terminal and read: hidden. b is unread, c and d run; newest change first.
	if !reflect.DeepEqual(got, []string{"b", "d", "c"}) {
		t.Fatalf("active list = %v", got)
	}
	if res.Counts != (Counts{Active: 2, Unread: 1, Total: 4}) {
		t.Fatalf("counts = %+v", res.Counts)
	}
	all, _ := f.m.List(ListQuery{Owner: "s1", State: "all"})
	if len(all.Items) != 4 {
		t.Fatalf("all = %d items", len(all.Items))
	}
	done, _ := f.m.List(ListQuery{Owner: "s1", State: "done"})
	if len(done.Items) != 2 {
		t.Fatalf("done = %d items", len(done.Items))
	}
	if _, err := f.m.List(ListQuery{Owner: "s1", State: "bogus"}); err == nil {
		t.Fatal("unknown state filter accepted")
	}
	other, _ := f.m.List(ListQuery{Owner: "s2", State: "all"})
	if len(other.Items) != 0 || other.Counts != (Counts{}) {
		t.Fatalf("another owner sees %+v", other)
	}
}

func TestListItemsCarryNoOutput(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: "SECRET-OUTPUT", Ref: "/r"})
	res, _ := f.m.List(ListQuery{Owner: "s1"})
	if strings.Contains(string(mustJSON(res)), "SECRET-OUTPUT") || strings.Contains(string(mustJSON(res)), "/r") {
		t.Fatalf("list leaked result text: %s", mustJSON(res))
	}
}

func TestListKindFilterAndBadCursor(t *testing.T) {
	f := newFixture(t, nil)
	other := &fakeDriver{kind: "other", m: f.m}
	if err := f.m.AddDriver(other); err != nil {
		t.Fatal(err)
	}
	f.start(t, "s1", "a")
	if err := f.m.Register(Registration{ID: "b", Owner: "s1", Kind: "other"}); err != nil {
		t.Fatal(err)
	}
	res, _ := f.m.List(ListQuery{Owner: "s1", Kind: "other"})
	if len(res.Items) != 1 || res.Items[0].ID != "b" {
		t.Fatalf("kind filter = %+v", res.Items)
	}
	if _, err := f.m.List(ListQuery{Owner: "s1", Cursor: "!!!"}); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}

// 1000 terminal handles: the list is paginated, bounded and stable, retention keeps the
// newest keep_finished, and the response is never above the byte budget.
func TestListPaginationAndRetentionAt1kHandles(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.KeepFinished = 200
		c.MaxHandles, c.MaxHandlesTotal = 2000, 2000
		c.UnreadMax = 5000
	})
	const total = 1000
	all := names("h", total)
	for _, id := range all {
		f.start(t, "s1", id)
		f.finish(t, id, Result{Summary: "s"})
	}
	// Read everything so retention applies.
	for {
		res, err := f.m.Wait(background, "s1", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Signals) == 0 {
			break
		}
	}
	f.m.Prune()
	counts := f.m.Counts("s1")
	if counts.Total != 200 || counts.Active != 0 || counts.Unread != 0 {
		t.Fatalf("counts after pruning = %+v, want total 200", counts)
	}
	files, _ := os.ReadDir(f.dir)
	records := 0
	for _, e := range files {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != indexFile {
			records++
		}
	}
	if records != 200 {
		t.Fatalf("%d record files on disk, want 200", records)
	}

	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("pagination does not end")
		}
		res, err := f.m.List(ListQuery{Owner: "s1", State: "all", Limit: 1000, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) > 50 {
			t.Fatalf("page of %d items exceeds the hard max", len(res.Items))
		}
		if encodedSize(res) > int(f.m.cfg.ResponseMaxBytes) {
			t.Fatalf("page is %d bytes", encodedSize(res))
		}
		for _, item := range res.Items {
			seen = append(seen, item.ID)
		}
		if res.Next == "" {
			break
		}
		cursor = res.Next
	}
	want := make([]string, 0, 200)
	for i := total - 1; i >= total-200; i-- {
		want = append(want, all[i])
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paginated ids differ: got %d, first %v, want newest-first 200", len(seen), seen[:min(3, len(seen))])
	}
}

func TestCursorSurvivesInsertsAndPruning(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.KeepFinished = 5 })
	for _, id := range names("h", 5) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{})
	}
	first, _ := f.m.List(ListQuery{Owner: "s1", State: "all", Limit: 2})
	if len(first.Items) != 2 || first.Next == "" || first.Items[0].ID != "h0004" {
		t.Fatalf("first page = %+v", first)
	}
	// New handles appear and the oldest rows are pruned between pages.
	f.start(t, "s1", "new")
	f.finish(t, "new", Result{})
	for range 3 {
		_, _ = f.m.Wait(background, "s1", nil, 0)
	}
	f.m.Prune()
	next, err := f.m.List(ListQuery{Owner: "s1", State: "all", Limit: 10, Cursor: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range next.Items {
		if item.ID == "new" || item.ID == "h0004" || item.ID == "h0003" {
			t.Fatalf("cursor page repeated or skipped ahead: %+v", next.Items)
		}
	}
}

func TestUnreadCapOverflowsOldestAndReportsDropped(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.UnreadMax, c.WaitMaxSignals = 3, 3 })
	for _, id := range names("h", 5) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{})
	}
	if got := f.m.Counts("s1"); got.Unread != 3 {
		t.Fatalf("unread = %d, want capped at 3", got.Unread)
	}
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"h0002", "h0003", "h0004"}) || res.Dropped != 2 {
		t.Fatalf("Wait = %v dropped %d", ids(res.Signals), res.Dropped)
	}
	// The overflowed result stays readable.
	if e, err := f.m.Status("s1", "h0000"); err != nil || e.State != StateDone {
		t.Fatalf("Status of an overflowed handle = %+v, %v", e, err)
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, "h0000.json"))
	if !strings.Contains(string(data), "\"overflow\":true") || strings.Contains(string(data), "\"unread\"") {
		t.Fatalf("overflow not recorded: %s", data)
	}
	if again, _ := f.m.Wait(background, "s1", nil, 0); again.Dropped != 0 {
		t.Fatalf("dropped reported twice: %d", again.Dropped)
	}
}

func TestOwnerEntryGoesWhenItsLastDroppedCountIsRead(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.UnreadMax, c.WaitMaxSignals = 1, 1 })
	for _, id := range names("h", 2) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{})
	}
	f.m.DeleteOwnersWithPrefix("s1", "gone")
	if _, ok := f.m.owners["s1"]; !ok {
		t.Fatal("the owner entry must still hold the dropped count until it is read")
	}
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if res.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", res.Dropped)
	}
	if _, ok := f.m.owners["s1"]; ok {
		t.Fatal("an owner with no rows and no dropped count left must not keep an entry")
	}
}

func TestUnreadTTLExpiresSignalsButKeepsResults(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "old")
	f.finish(t, "old", Result{})
	f.clock.Advance(8 * 24 * time.Hour)
	f.start(t, "s1", "new")
	f.finish(t, "new", Result{})
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"new"}) || res.Dropped != 1 {
		t.Fatalf("Wait = %v dropped %d, want the expired signal gone", ids(res.Signals), res.Dropped)
	}
	if e, err := f.m.Status("s1", "old"); err != nil || e.State != StateDone {
		t.Fatalf("expired signal's result = %+v, %v", e, err)
	}
}

func TestRetentionDaysRemovesOldReadRecordsOnly(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "old")
	f.finish(t, "old", Result{})
	f.start(t, "s1", "live")
	_, _ = f.m.Wait(background, "s1", []string{"old"}, 0)
	f.clock.Advance(15 * 24 * time.Hour)
	f.m.Prune()
	if _, err := f.m.Status("s1", "old"); err == nil {
		t.Fatal("a record past retention_days survived")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "old.json")); !os.IsNotExist(err) {
		t.Fatalf("record file not removed: %v", err)
	}
	if e, err := f.m.Status("s1", "live"); err != nil || e.State != StateRunning {
		t.Fatalf("live handle pruned: %+v, %v", e, err)
	}
}

func TestUnreadSignalsAreNeverPrunedByCount(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.KeepFinished = 2 })
	for _, id := range names("h", 6) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{})
	}
	f.m.Prune()
	if got := f.m.Counts("s1"); got.Total != 6 || got.Unread != 6 {
		t.Fatalf("counts = %+v, unread handles must survive the count rule", got)
	}
}

func TestDropUnreadMarksReadAndKeepsResults(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.start(t, "s2", "b")
	f.finish(t, "a", Result{})
	f.finish(t, "b", Result{})
	if n := f.m.DropUnread("s1"); n != 1 {
		t.Fatalf("DropUnread = %d", n)
	}
	if res, _ := f.m.Wait(background, "s1", nil, 0); len(res.Signals) != 0 {
		t.Fatalf("dropped signal delivered: %+v", res)
	}
	if res, _ := f.m.Wait(background, "s2", nil, 0); len(res.Signals) != 1 {
		t.Fatalf("another owner's signal dropped: %+v", res)
	}
	if e, err := f.m.Status("s1", "a"); err != nil || e.State != StateDone {
		t.Fatalf("result after DropUnread = %+v, %v", e, err)
	}
}

func TestCancelOwnerAndPrefixFanOut(t *testing.T) {
	f := newFixture(t, nil)
	other := &fakeDriver{kind: "other", m: f.m}
	_ = f.m.AddDriver(other)
	f.start(t, "sess|a", "a1")
	f.start(t, "sess|b", "b1")
	f.start(t, "zzz|a", "z1")
	if err := f.m.Register(Registration{ID: "a2", Owner: "sess|a", Kind: "other"}); err != nil {
		t.Fatal(err)
	}
	f.finish(t, "a1", Result{})
	if n := f.m.CancelOwner("sess|a", "user_stop"); n != 1 {
		t.Fatalf("CancelOwner asked %d, want only the live handle of the other kind", n)
	}
	if got := other.stopped(); !reflect.DeepEqual(got, []string{"a2:user_stop"}) {
		t.Fatalf("other driver stops = %v", got)
	}
	if n := f.m.CancelOwnersWithPrefix("sess|", "session_delete"); n != 2 {
		t.Fatalf("prefix cancel asked %d, want the 2 live handles of the session (a2, b1)", n)
	}
	for _, s := range f.driver.stopped() {
		if strings.HasPrefix(s, "z1") {
			t.Fatal("prefix cancel reached another session")
		}
	}
}

func TestCancelDoesNotBlockOnASlowDriver(t *testing.T) {
	f := newFixture(t, nil)
	block := make(chan struct{})
	slow := &blockingDriver{kind: "slow", release: block}
	_ = f.m.AddDriver(slow)
	if err := f.m.Register(Registration{ID: "s", Owner: "o", Kind: "slow"}); err != nil {
		t.Fatal(err)
	}
	// The driver is called without the manager lock, so the manager stays usable.
	done := make(chan struct{})
	go func() { f.m.CancelOwner("o", "r"); close(done) }()
	poll(t, "driver Stop to start", func() bool { return slow.entered.Load() })
	if got := f.m.Counts("o"); got.Active != 1 {
		t.Fatalf("Counts blocked or wrong while a driver stops: %+v", got)
	}
	close(block)
	<-done
}

func TestRestartMarksRunningLostAndQueuesSignal(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "run")
	f.start(t, "s1", "fin")
	f.start(t, "s1", "read")
	f.finish(t, "fin", Result{Summary: "kept", Ref: "/r"})
	f.finish(t, "read", Result{})
	_, _ = f.m.Wait(background, "s1", []string{"read"}, 0)
	f.abandon()

	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.open(t)
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if !reflect.DeepEqual(ids(res.Signals), []string{"fin", "run"}) {
		t.Fatalf("signals after restart = %v", ids(res.Signals))
	}
	if res.Signals[0].Summary != "kept" || res.Signals[0].Ref != "/r" {
		t.Fatalf("undelivered result lost its text: %+v", res.Signals[0])
	}
	if e := res.Signals[1]; e.State != StateLost || e.OK || e.Reason != ReasonRestarted {
		t.Fatalf("lost entry = %+v", e)
	}
	if e, err := f.m.Status("s1", "read"); err != nil || e.State != StateDone {
		t.Fatalf("read terminal handle after restart = %+v, %v", e, err)
	}
	if got := f.m.Counts("s1"); got.Active != 0 {
		t.Fatalf("live count after restart = %+v", got)
	}
}

func TestRestartLeavesALiveForeignServersHandlesAlone(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "theirs")
	// Server 100 is still running: it has not closed its manager.
	f.abandon()
	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.opts.ServerAlive = func(id ServerID) bool { return id.PID == 100 }
	f.open(t)
	if e, err := f.m.Status("s1", "theirs"); err != nil || e.State != StateRunning {
		t.Fatalf("foreign live handle = %+v, %v", e, err)
	}
}

// A handle whose driver does not report within StopWait ends at Close as lost, unread, so
// its record is not left running: the next server shows its signal even while this process
// is still alive, and a late report of the driver is ignored.
func TestCloseEndsAnUnreportedHandleAsLost(t *testing.T) {
	f := newFixture(t, nil)
	f.opts.StopWait = 0
	f.m.Close()
	f.open(t)
	f.start(t, "s1", "slow")
	f.m.Close()
	if ok, err := f.m.Complete("slow", Result{State: StateDone, OK: true}); err != nil || ok {
		t.Fatalf("late report after Close = %v, %v; want ignored", ok, err)
	}
	rec, err := f.m.readRecord("slow")
	if err != nil || rec.State != StateLost || rec.Reason != ReasonShutdown || !rec.Unread || rec.EndedAt.IsZero() {
		t.Fatalf("record after Close = %+v, %v", rec, err)
	}
	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.opts.ServerAlive = func(id ServerID) bool { return id.PID == 100 }
	f.open(t)
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != 1 || res.Signals[0].State != StateLost || res.Signals[0].Reason != ReasonShutdown {
		t.Fatalf("signals after restart = %+v", res.Signals)
	}
	if got := f.m.Counts("s1"); got.Active != 0 {
		t.Fatalf("live count after restart = %+v", got)
	}
}

// Close ends only this server's handles: one that another live server owns stays running.
func TestCloseLeavesALiveForeignServersHandleRunning(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "theirs")
	f.abandon()
	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.opts.ServerAlive = func(id ServerID) bool { return id.PID == 100 }
	f.opts.StopWait = 0
	f.open(t)
	f.m.Close()
	if rec, err := f.m.readRecord("theirs"); err != nil || rec.State != StateRunning {
		t.Fatalf("foreign record after Close = %+v, %v", rec, err)
	}
}

// Close stops and waits for only this server's handles: one that another live server owns
// never reports to this one, so it gets no Stop and does not hold Close for the whole stop
// wait.
func TestCloseDoesNotWaitForALiveForeignServersHandle(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "theirs")
	f.abandon()
	f.opts.Server = ServerID{PID: 200, Start: 2}
	f.opts.ServerAlive = func(id ServerID) bool { return id.PID == 100 }
	f.opts.StopWait = 10 * time.Second
	f.open(t)
	f.start(t, "s1", "mine")
	f.finish(t, "mine", Result{})
	began := time.Now()
	f.m.CloseWithin(20 * time.Second)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Close took %v with only a foreign handle left live", took)
	}
	if got := f.driver.stopped(); len(got) != 0 {
		t.Fatalf("stops = %v, want none: the live handle is another server's", got)
	}
	if rec, err := f.m.readRecord("mine"); err != nil || rec.State != StateDone {
		t.Fatalf("own record after Close = %+v, %v", rec, err)
	}
	if rec, err := f.m.readRecord("theirs"); err != nil || rec.State != StateRunning {
		t.Fatalf("foreign record after Close = %+v, %v", rec, err)
	}
}

// A Complete that claimed its handle before Close's stop wait ran out has its result on disk,
// and in the final index, once Close returns: Close waits for it instead of shutting the
// folder under it and leaving the record running.
func TestCloseWaitsForACompleteThatClaimedItsHandle(t *testing.T) {
	f := newFixture(t, nil)
	f.opts.StopWait = 20 * time.Millisecond
	f.m.Close()
	f.open(t)
	f.start(t, "s1", "late")
	claimed, release := make(chan struct{}), make(chan struct{})
	f.m.beforeCompleteWrite = func(string) {
		close(claimed)
		<-release
	}
	completed := make(chan bool, 1)
	go func() {
		ok, _ := f.m.Complete("late", Result{State: StateDone, OK: true, Summary: "the result"})
		completed <- ok
	}()
	<-claimed
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		f.m.Close()
	}()
	// Long past the stop wait: a Close that does not wait for the claimed Complete is done.
	select {
	case <-closed:
	case <-time.After(10 * f.opts.StopWait):
	}
	close(release)
	<-closed
	if !<-completed {
		t.Fatal("the claimed Complete did not win")
	}
	rec, err := f.m.readRecord("late")
	if err != nil || rec.State != StateDone || rec.Summary != "the result" || !rec.Unread {
		t.Fatalf("record after Close = %+v, %v; want the reported result, unread", rec, err)
	}
	if row, ok := indexRowOf(t, f, "late"); !ok || row.State != StateDone || !row.Unread {
		t.Fatalf("index row after Close = %+v (present %v)", row, ok)
	}
}

// A change that marks signals read or removes records holds the file lock until its files
// are written, so a Close that comes in between finds it on disk and in the final index;
// before, the change could land only in memory and the signal came back after a restart.
func TestCloseDuringAChangeFindsItOnDisk(t *testing.T) {
	unread := func(f *fixture) (present, unread bool) {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		r := f.m.rows["fin"]
		return r != nil, r != nil && r.unread
	}
	read := func(f *fixture) bool { present, u := unread(f); return present && !u }
	gone := func(f *fixture) bool { present, _ := unread(f); return !present }
	for _, tc := range []struct {
		name    string
		action  func(*Manager)
		changed func(*fixture) bool
		removed bool
	}{
		{"drop unread", func(m *Manager) { m.DropUnread("s1") }, read, false},
		{"mark read", func(m *Manager) { m.MarkRead("s1", "fin") }, read, false},
		{"wait delivery", func(m *Manager) { _, _ = m.Wait(background, "s1", nil, 0) }, read, false},
		{"delete", func(m *Manager) { m.DeleteOwnersWithPrefix("s1", "session_delete") }, gone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.start(t, "s1", "fin")
			f.finish(t, "fin", Result{})
			var closed chan struct{}
			f.m.beforeCommitWrite = func() {
				if closed != nil || !tc.changed(f) {
					return
				}
				closed = make(chan struct{})
				go func() {
					defer close(closed)
					f.m.Close()
				}()
				// Close blocks on the file lock this change holds; without it, it ends here.
				select {
				case <-closed:
				case <-time.After(100 * time.Millisecond):
				}
			}
			tc.action(f.m)
			if closed == nil {
				t.Fatal("the change never reached its file writes")
			}
			<-closed
			row, inIndex := indexRowOf(t, f, "fin")
			if tc.removed {
				if _, err := os.Stat(filepath.Join(f.dir, "fin.json")); !os.IsNotExist(err) {
					t.Fatalf("the removed record is still on disk (%v)", err)
				}
				if inIndex {
					t.Fatalf("the final index still lists the removed handle: %+v", row)
				}
				return
			}
			if rec, err := f.m.readRecord("fin"); err != nil || rec.Unread {
				t.Fatalf("record on disk = %+v, %v; want read", rec, err)
			}
			if !inIndex || row.Unread {
				t.Fatalf("index row = %+v (present %v); want read", row, inIndex)
			}
		})
	}
}

// indexRowOf reads id's row from index.json as written on disk.
func indexRowOf(t *testing.T, f *fixture, id string) (indexRow, bool) {
	t.Helper()
	rows, ok := f.m.readIndex()
	if !ok {
		t.Fatal("index.json is missing or unreadable")
	}
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return indexRow{}, false
}

func TestRestartWithCorruptOrMissingIndexRebuilds(t *testing.T) {
	for name, damage := range map[string]func(string){
		"missing": func(p string) { _ = os.Remove(p) },
		"garbage": func(p string) { _ = os.WriteFile(p, []byte("{not json"), 0o644) },
		"checksum": func(p string) {
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.Replace(string(b), "\"sum\":\"", "\"sum\":\"0", 1)), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			for _, id := range names("h", 5) {
				f.start(t, "s1", id)
				f.finish(t, id, Result{})
			}
			_, _ = f.m.Wait(background, "s1", names("h", 3), 0)
			f.start(t, "s1", "run")
			f.m.Close()
			damage(filepath.Join(f.dir, indexFile))
			f.opts.Server = ServerID{PID: 200, Start: 2}
			f.open(t)
			if got := f.m.Counts("s1"); got.Total != 6 || got.Active != 0 || got.Unread != 3 {
				t.Fatalf("rebuilt counts = %+v", got)
			}
			if _, err := os.Stat(filepath.Join(f.dir, indexFile)); err != nil {
				t.Fatalf("index not rewritten: %v", err)
			}
		})
	}
}

func TestRestartOpensOnlyNonTerminalRecordsFromTheIndex(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.KeepFinished = 500; c.UnreadMax = 500 })
	for _, id := range names("h", 100) {
		f.start(t, "s1", id)
		f.finish(t, id, Result{})
	}
	for {
		if res, _ := f.m.Wait(background, "s1", nil, 0); len(res.Signals) == 0 {
			break
		}
	}
	f.m.Close()
	// Corrupt every terminal, read record. A start that read them would drop rows.
	for _, id := range names("h", 100) {
		_ = os.WriteFile(filepath.Join(f.dir, id+".json"), []byte("junk"), 0o644)
	}
	f.open(t)
	if got := f.m.Counts("s1"); got.Total != 100 {
		t.Fatalf("restart read the terminal history: %+v", got)
	}
}

func TestRecordsAreSmallAndHoldNoLargeResult(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: strings.Repeat("x", 1<<20), Ref: strings.Repeat("r", 4096), Code: strings.Repeat("c", 999)})
	info, err := os.Stat(filepath.Join(f.dir, "a.json"))
	if err != nil || info.Size() > 2048 {
		t.Fatalf("record file: %v size %v, want < 2 KB", err, info)
	}
}

func TestOversizedEntryHasItsSummaryShortenedToTheBudget(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SummaryMaxBytes, c.StatusSummaryBytes = 2000, 2000; c.ResponseMaxBytes = 3100 })
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: strings.Repeat("x", 3000), Ref: strings.Repeat("r", 500)})
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != 1 || encodedSize(res) > 3100 || !res.Signals[0].Cut || res.Signals[0].Ref == "" {
		t.Fatalf("signals %d size %d: %+v", len(res.Signals), encodedSize(res), res.Signals)
	}
}

func TestCloseStopsLiveHandlesWakesWaitersAndRefusesWork(t *testing.T) {
	f := newFixture(t, nil)
	f.driver.autoComplete = true
	f.start(t, "s1", "a")
	f.start(t, "s2", "b")
	f.finish(t, "b", Result{})
	errs := make(chan error, 1)
	go func() {
		_, err := f.m.Wait(background, "s9", nil, time.Minute)
		errs <- err
	}()
	f.start(t, "s9", "c")
	time.Sleep(20 * time.Millisecond)
	f.m.Close()
	f.m.Close()
	if err := <-errs; err != ErrClosed && err != nil {
		t.Fatalf("waiter error = %v", err)
	}
	if got := f.driver.stopped(); !reflect.DeepEqual(got, []string{"a:" + ReasonShutdown, "c:" + ReasonShutdown}) {
		t.Fatalf("Close stops = %v", got)
	}
	if err := f.m.Register(Registration{ID: "n", Owner: "s1", Kind: "fake"}); err != ErrClosed {
		t.Fatalf("Register after Close = %v", err)
	}
	// The shutdown stops are queued for the next start.
	f.opts.Server = ServerID{PID: 300, Start: 3}
	f.open(t)
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != 1 || res.Signals[0].State != StateStopped || res.Signals[0].Reason != ReasonShutdown {
		t.Fatalf("shutdown signal after restart = %+v", res.Signals)
	}
}

// Many goroutines complete, wait and stop at once. Run with -race. Every handle is
// delivered exactly once, and caps never go below zero.
func TestConcurrentCompleteWaitStopRace(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.MaxHandles, c.MaxHandlesTotal, c.KeepFinished, c.UnreadMax = 1000, 1000, 1000, 1000
	})
	f.driver.autoComplete = true
	const owners, perOwner = 4, 50
	var delivered, seen sync.Map
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for o := range owners {
		owner := names("o", owners)[o]
		for _, id := range names(owner+"-", perOwner) {
			f.start(t, owner, id)
		}
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					res, err := f.m.Wait(background, owner, nil, 50*time.Millisecond)
					if err != nil {
						t.Error(err)
						return
					}
					for _, e := range res.Signals {
						if _, dup := delivered.LoadOrStore(e.ID, true); dup {
							t.Errorf("signal %s delivered twice", e.ID)
						}
						seen.Store(e.ID, true)
					}
					select {
					case <-stop:
						return
					default:
					}
				}
			}()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i, id := range names(owner+"-", perOwner) {
				if i%2 == 0 {
					_, _ = f.m.Complete(id, Result{State: StateDone, OK: true})
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i, id := range names(owner+"-", perOwner) {
				if i%2 == 1 {
					if e, err := f.m.Stop(background, owner, id, "x"); err == nil && e.State.Terminal() {
						seen.Store(e.ID, true)
					}
				}
				_, _ = f.m.List(ListQuery{Owner: owner, State: "all"})
				_, _ = f.m.Status(owner, id)
			}
			f.m.CancelOwner(owner, "late")
		}()
	}
	poll(t, "all signals delivered", func() bool {
		n := 0
		seen.Range(func(_, _ any) bool { n++; return true })
		return n == owners*perOwner
	})
	close(stop)
	wg.Wait()
	for o := range owners {
		if got := f.m.Counts(names("o", owners)[o]); got.Active != 0 || got.Unread != 0 {
			t.Errorf("owner %d counts = %+v", o, got)
		}
	}
	f.m.mu.Lock()
	live := f.m.liveTotal
	f.m.mu.Unlock()
	if live != 0 {
		t.Fatalf("liveTotal = %d after everything ended", live)
	}
}

// After N handles have lived their whole life the manager leaves no goroutine behind.
func TestNoGoroutineLeakAfterManyHandles(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxHandles, c.MaxHandlesTotal, c.KeepFinished, c.UnreadMax = 500, 500, 500, 500 })
	f.driver.autoComplete = true
	before := settledGoroutines()
	for _, id := range names("h", 300) {
		f.start(t, "s1", id)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.m.Wait(background, "s1", nil, 20*time.Millisecond)
		}()
	}
	for i, id := range names("h", 300) {
		if i%2 == 0 {
			f.finish(t, id, Result{})
		} else {
			_, _ = f.m.Stop(background, "s1", id, "r")
		}
	}
	wg.Wait()
	f.m.Close()
	poll(t, "goroutines to settle", func() bool { return settledGoroutines() <= before })
}

func settledGoroutines() int {
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	return runtime.NumGoroutine()
}

// The index must not make startup read every record (see TestRestartOpensOnly...). This
// benchmark compares startup from a valid index with a rebuild from the records.
func BenchmarkOpenFromIndex(b *testing.B) { benchmarkOpen(b, false) }
func BenchmarkOpenRebuild(b *testing.B)   { benchmarkOpen(b, true) }

func benchmarkOpen(b *testing.B, rebuild bool) {
	cfg := DefaultConfig()
	cfg.KeepFinished, cfg.UnreadMax = 10_000, 10_000
	cfg.MaxHandles, cfg.MaxHandlesTotal = 20_000, 20_000
	clock := &fakeClock{t: time.Now()}
	dir := b.TempDir()
	opts := Options{Dir: dir, Config: cfg, Server: ServerID{PID: 1, Start: 1}, Now: clock.Now}
	m, err := Open(opts)
	if err != nil {
		b.Fatal(err)
	}
	d := &fakeDriver{kind: "fake", m: m}
	_ = m.AddDriver(d)
	const total = 5000
	for _, id := range names("h", total) {
		_ = m.Register(Registration{ID: id, Owner: "s1", Kind: "fake"})
		_, _ = m.Complete(id, Result{State: StateDone, OK: true})
	}
	for {
		if res, _ := m.Wait(background, "s1", nil, 0); len(res.Signals) == 0 {
			break
		}
	}
	m.Close()
	if rebuild {
		_ = os.Remove(filepath.Join(dir, indexFile))
	}
	b.ResetTimer()
	for range b.N {
		if rebuild {
			_ = os.Remove(filepath.Join(dir, indexFile))
		}
		m2, err := Open(opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = m2
	}
}

func BenchmarkListPage10Of10kRows(b *testing.B) {
	cfg := DefaultConfig()
	cfg.KeepFinished, cfg.UnreadMax = 10_000, 10_000
	cfg.MaxHandles, cfg.MaxHandlesTotal = 20_000, 20_000
	clock := &fakeClock{t: time.Now()}
	m, _ := Open(Options{Dir: b.TempDir(), Config: cfg, Now: clock.Now})
	_ = m.AddDriver(&fakeDriver{kind: "fake", m: m})
	for _, id := range names("h", 10_000) {
		_ = m.Register(Registration{ID: id, Owner: "s1", Kind: "fake"})
		_, _ = m.Complete(id, Result{State: StateDone, OK: true})
	}
	b.ResetTimer()
	for range b.N {
		if _, err := m.List(ListQuery{Owner: "s1", State: "all"}); err != nil {
			b.Fatal(err)
		}
	}
}
