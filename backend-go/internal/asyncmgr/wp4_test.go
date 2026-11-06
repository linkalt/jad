package asyncmgr

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// duringCommit runs action (a reset, delete or close) on its own goroutine from inside
// Complete, between its result write and its commit, where Complete holds ioMu. It
// returns once ready reports true, and a channel closed when action has returned.
func duringCommit(action func(), ready func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		action()
	}()
	for !ready() {
		time.Sleep(time.Millisecond)
	}
	return done
}

// closing reports that Close has started.
func (f *fixture) closing() bool {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return f.m.closed
}

// finishingAfter reports the end action a reset or delete set on the finishing row id.
func (f *fixture) finishingAfter(id string) endAction {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if r := f.m.rows[id]; r != nil {
		return r.after
	}
	return endKeep
}

func TestResetOwnerStopsItsLiveHandlesAndTheirEndIsBornRead(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "done1")
	f.finish(t, "done1", Result{})
	f.start(t, "s1", "live1")
	f.start(t, "s2", "live2")
	if n := f.m.ResetOwner("s1", "session_reset"); n != 1 {
		t.Fatalf("ResetOwner asked %d, want 1", n)
	}
	if got := f.driver.stopped(); !reflect.DeepEqual(got, []string{"live1:session_reset"}) {
		t.Fatalf("stops = %v", got)
	}
	f.finish(t, "live1", Result{State: StateStopped, Reason: "session_reset"})
	if res, _ := f.m.Wait(background, "s1", nil, 0); len(res.Signals) != 0 {
		t.Fatalf("a reset delivered a signal: %+v", res.Signals)
	}
	if e, err := f.m.Status("s1", "live1"); err != nil || e.State != StateStopped || e.Reason != "session_reset" {
		t.Fatalf("stopped handle after reset = %+v, %v", e, err)
	}
	if c := f.m.Counts("s1"); c.Unread != 0 {
		t.Fatalf("unread after reset = %d", c.Unread)
	}
	f.finish(t, "live2", Result{})
	if res, _ := f.m.Wait(background, "s2", nil, 0); len(res.Signals) != 1 {
		t.Fatalf("another owner's handle was affected: %+v", res)
	}
}

func TestCancelOwnerStillQueuesTheStopSignal(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.m.CancelOwner("s1", "user_stop")
	f.finish(t, "a", Result{State: StateStopped, Reason: "user_stop"})
	res, _ := f.m.Wait(background, "s1", nil, 0)
	if len(res.Signals) != 1 || res.Signals[0].State != StateStopped || res.Signals[0].Reason != "user_stop" {
		t.Fatalf("stop signal = %+v", res.Signals)
	}
}

func TestCancelOwnerAfterResetKeepsTheResetsBornReadEnd(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "live1")
	f.m.ResetOwner("s1", "session_reset")
	f.m.CancelOwner("s1", "user_stop")
	f.finish(t, "live1", Result{State: StateStopped, Reason: "session_reset"})
	if res, _ := f.m.Wait(background, "s1", nil, 0); len(res.Signals) != 0 {
		t.Fatalf("a later CancelOwner made the reset's end unread: %+v", res.Signals)
	}
}

func TestDeleteOwnersWithPrefixStopsLiveRemovesFinishedAndSparesOthers(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "sess|a", "fin")
	f.finish(t, "fin", Result{})
	f.start(t, "sess|a", "live1")
	f.start(t, "sess|b", "live2")
	f.start(t, "other|a", "keep")
	if n := f.m.DeleteOwnersWithPrefix("sess|", "session_delete"); n != 2 {
		t.Fatalf("asked %d, want 2", n)
	}
	if _, err := f.m.Status("sess|a", "fin"); err == nil {
		t.Fatal("finished handle of the deleted session still known")
	}
	f.finish(t, "live1", Result{State: StateStopped, Reason: "session_delete"})
	f.finish(t, "live2", Result{State: StateStopped, Reason: "session_delete"})
	for _, id := range []string{"fin", "live1", "live2"} {
		if _, err := os.Stat(filepath.Join(f.dir, id+".json")); err == nil {
			t.Fatalf("record %s still on disk", id)
		}
	}
	if c := f.m.Counts("sess|a"); c.Total != 0 || c.Unread != 0 {
		t.Fatalf("counts of the deleted session = %+v", c)
	}
	if e, err := f.m.Status("other|a", "keep"); err != nil || e.State.Terminal() {
		t.Fatalf("another session's handle = %+v, %v", e, err)
	}
	for _, s := range f.driver.stopped() {
		if s == "keep:session_delete" {
			t.Fatal("delete reached another session")
		}
	}
}

// A handle whose end is being recorded when its session is deleted is removed with the
// session: its signal must not stay behind under the deleted owner.
func TestDeleteWhileAHandleIsFinishingRemovesIt(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "sess|a", "fin")
	var deleted <-chan struct{}
	f.m.beforeCompleteCommit = func(string) {
		f.m.beforeCompleteCommit = nil
		deleted = duringCommit(func() {
			if n := f.m.DeleteOwnersWithPrefix("sess|", "session_delete"); n != 0 {
				t.Errorf("delete asked %d to stop, want 0: the handle is already ending", n)
			}
		}, func() bool { return f.finishingAfter("fin") == endRemove })
	}
	f.finish(t, "fin", Result{})
	<-deleted
	if _, err := f.m.Status("sess|a", "fin"); err == nil {
		t.Fatal("the finishing handle of the deleted session is still known")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "fin.json")); err == nil {
		t.Fatal("its record is still on disk")
	}
	if c := f.m.Counts("sess|a"); c.Total != 0 || c.Unread != 0 {
		t.Fatalf("counts of the deleted session = %+v", c)
	}
	if got := f.driver.stopped(); len(got) != 0 {
		t.Fatalf("stops = %v, want none", got)
	}
}

// The same for a reset: the end of a handle that was finishing during it is born read, in
// memory and in its record.
func TestResetWhileAHandleIsFinishingMakesItsEndRead(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "fin")
	var reset <-chan struct{}
	f.m.beforeCompleteCommit = func(string) {
		f.m.beforeCompleteCommit = nil
		reset = duringCommit(func() { f.m.ResetOwner("s1", "session_reset") },
			func() bool { return f.finishingAfter("fin") == endRead })
	}
	f.finish(t, "fin", Result{})
	<-reset
	if res, _ := f.m.Wait(background, "s1", nil, 0); len(res.Signals) != 0 {
		t.Fatalf("a reset left the finishing handle's signal: %+v", res.Signals)
	}
	if c := f.m.Counts("s1"); c.Unread != 0 {
		t.Fatalf("unread after reset = %d", c.Unread)
	}
	rec, err := f.m.readRecord("fin")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Unread || rec.State != StateDone {
		t.Fatalf("record on disk = unread %v state %s, want read and done", rec.Unread, rec.State)
	}
}

// A shutdown that comes while the end of a reset or deleted handle is being recorded does
// not leave its record unread on disk, where the next start would queue it again. Close does
// not wait for the write here (StopWait 0), so only the file lock keeps it out.
func TestCloseWhileAResetOrDeletedHandleEndsLeavesNoUnreadRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(*Manager)
		want   endAction
	}{
		{"reset", func(m *Manager) { m.ResetOwner("s1", "session_reset") }, endRead},
		{"delete", func(m *Manager) { m.DeleteOwnersWithPrefix("s1", "session_delete") }, endRemove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.opts.StopWait = 0
			f.open(t)
			f.start(t, "s1", "fin")
			var acted, closed <-chan struct{}
			f.m.beforeCompleteCommit = func(string) {
				f.m.beforeCompleteCommit = nil
				acted = duringCommit(func() { tc.action(f.m) },
					func() bool { return f.finishingAfter("fin") == tc.want })
				closed = duringCommit(f.m.Close, f.closing)
				// Close waits for this Complete and for the file lock it holds; without them,
				// it ends here.
				select {
				case <-closed:
				case <-time.After(100 * time.Millisecond):
				}
			}
			f.finish(t, "fin", Result{})
			<-acted
			<-closed
			rec, err := f.m.readRecord("fin")
			switch tc.want {
			case endRead:
				if err != nil || rec.Unread || rec.State != StateDone {
					t.Fatalf("record on disk = unread %v state %s (%v), want read and done", rec.Unread, rec.State, err)
				}
				if row, ok := indexRowOf(t, f, "fin"); !ok || row.Unread || row.State != StateDone {
					t.Fatalf("final index row = %+v (present %v), want read and done", row, ok)
				}
			case endRemove:
				if _, err := os.Stat(filepath.Join(f.dir, "fin.json")); !os.IsNotExist(err) {
					t.Fatalf("the deleted handle's record is still on disk (%v)", err)
				}
				// The final index is written after this Complete, so it no longer lists the
				// handle and the next start has no stale row to drop.
				if row, ok := indexRowOf(t, f, "fin"); ok {
					t.Fatalf("the final index still lists the deleted handle: %+v", row)
				}
			}
		})
	}
}
