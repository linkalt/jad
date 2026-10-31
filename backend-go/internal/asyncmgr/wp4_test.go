package asyncmgr

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
