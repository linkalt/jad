package asyncmgr

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// CancelLineage reaches the live handles whose lineage holds a matching key, whoever owns them;
// it keeps their records and signals, and the lineage is never written to a record.
func TestCancelLineageReachesLiveHandlesByLineageOnly(t *testing.T) {
	f := newFixture(t, nil)
	f.driver.autoComplete = true
	register := func(id, owner string, lineage ...string) {
		t.Helper()
		if err := f.m.Register(Registration{ID: id, Owner: owner, Kind: "fake", Label: "job " + id, Lineage: lineage}); err != nil {
			t.Fatal(err)
		}
	}
	register("a", "s2", "demo\x00dev", "profile_run_1")
	register("b", "demo\x00dev")
	register("c", "s3", "profile_run_1")
	register("d", "s4", "profile_run_2")
	f.finish(t, "c", Result{Summary: "done before"})
	if n := f.m.CancelLineage(func(key string) bool { return key == "profile_run_1" }, "caller_stopped"); n != 1 {
		t.Fatalf("CancelLineage = %d, want the one live handle under the key", n)
	}
	if n := f.m.CancelLineage(func(key string) bool { return strings.HasPrefix(key, "demo\x00") }, "session_delete"); n != 0 {
		t.Fatalf("a stopped handle or the owner's own one was reached again: %d", n)
	}
	if got := f.driver.stopped(); !reflect.DeepEqual(got, []string{"a:caller_stopped"}) {
		t.Fatalf("driver stops = %v", got)
	}
	if e, err := f.m.Status("s2", "a"); err != nil || e.State != StateStopped || e.Reason != "caller_stopped" {
		t.Fatalf("a = %+v %v", e, err)
	}
	if got := f.m.Counts("s2"); got.Unread != 1 {
		t.Fatalf("the stopped handle's signal was dropped: %+v", got)
	}
	err := filepath.WalkDir(f.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "profile_run_") || strings.Contains(strings.ToLower(string(data)), "lineage") {
			t.Errorf("%s holds the lineage:\n%s", path, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// keyedDriver is a fakeDriver that also takes the handle's Key (KeyedStopper).
type keyedDriver struct{ *fakeDriver }

func (d keyedDriver) StopKeyed(id, key, reason string) { d.Stop(id, key+"/"+reason) }

// A KeyedStopper's stop is given the handle's Key on a cancel and on a model's stop; the key is
// kept in memory only, like the lineage.
func TestAKeyedStopperGetsTheHandlesKey(t *testing.T) {
	f := newFixture(t, nil)
	keyed := keyedDriver{&fakeDriver{kind: "keyed", m: f.m, autoComplete: true}}
	if err := f.m.AddDriver(keyed); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := f.m.Register(Registration{ID: id, Owner: "s1", Kind: "keyed", Label: "job " + id, Key: "subagent:" + id + "#1"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.m.CancelOwner("s1", "user_stop"); n != 2 {
		t.Fatalf("CancelOwner = %d", n)
	}
	if err := f.m.Register(Registration{ID: "c", Owner: "s1", Kind: "keyed", Label: "job c", Key: "subagent:c#2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Stop(context.Background(), "s1", "c", "stopped_by_agent"); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:subagent:a#1/user_stop", "b:subagent:b#1/user_stop", "c:subagent:c#2/stopped_by_agent"}
	if got := keyed.stopped(); !reflect.DeepEqual(got, want) {
		t.Fatalf("driver stops = %v, want %v", got, want)
	}
}

// DeleteLineage deletes the handles under the key that the deleted session owns (a live one when it
// ends, a finished one at once, records included), only stops those of other owners and keeps
// their records, and leaves handles outside the lineage alone. A later CancelLineage does not undo
// the removal (the strongest end wins).
func TestDeleteLineageRemovesOnlyTheDeletedSessionsHandles(t *testing.T) {
	f := newFixture(t, nil)
	register := func(id, owner string, lineage ...string) {
		t.Helper()
		if err := f.m.Register(Registration{ID: id, Owner: owner, Kind: "fake", Label: "job " + id, Lineage: lineage}); err != nil {
			t.Fatal(err)
		}
	}
	register("own", "demo\x00dev", "session_run#1")
	register("done", "demo\x00dev", "session_run#1")
	register("other", "s2\x00dev", "session_run#1")
	register("outside", "demo\x00dev", "session_run#2")
	f.finish(t, "done", Result{Summary: "done before"})
	key := func(k string) bool { return k == "session_run#1" }
	if n := f.m.DeleteLineage(key, "demo\x00", "session_delete"); n != 2 {
		t.Fatalf("DeleteLineage = %d, want the two live handles under the key", n)
	}
	if n := f.m.CancelLineage(key, "user_stop"); n != 2 {
		t.Fatalf("CancelLineage = %d, want the two handles still live", n)
	}
	if _, err := f.m.Status("demo\x00dev", "done"); err == nil {
		t.Fatal("the finished handle of the deleted session was kept")
	}
	if _, err := os.Stat(f.m.recordPath("done")); !os.IsNotExist(err) {
		t.Fatalf("the finished handle's record file is kept: %v", err)
	}
	if e, err := f.m.Status("demo\x00dev", "own"); err != nil || e.State.Terminal() {
		t.Fatalf("own before its end = %+v %v, want live until the driver ends it", e, err)
	}
	f.finish(t, "own", Result{State: StateStopped, Reason: "session_delete"})
	f.finish(t, "other", Result{State: StateStopped, Reason: "session_delete"})
	if _, err := f.m.Status("demo\x00dev", "own"); err == nil {
		t.Fatal("the deleted session's handle was kept after it ended")
	}
	if _, err := os.Stat(f.m.recordPath("own")); !os.IsNotExist(err) {
		t.Fatalf("the deleted session's record file is kept: %v", err)
	}
	if e, err := f.m.Status("s2\x00dev", "other"); err != nil || e.State != StateStopped {
		t.Fatalf("other = %+v %v, want stopped with its record kept", e, err)
	}
	if got := f.m.Counts("s2\x00dev"); got.Unread != 1 {
		t.Fatalf("the other owner's signal was dropped: %+v", got)
	}
	if e, err := f.m.Status("demo\x00dev", "outside"); err != nil || e.State != StateRunning {
		t.Fatalf("outside = %+v %v, want running", e, err)
	}
	if got := f.driver.stopped(); !reflect.DeepEqual(got, []string{"other:session_delete", "own:session_delete", "other:user_stop", "own:user_stop"}) {
		t.Fatalf("driver stops = %v", got)
	}
}

// HasLiveLineage sees only live handles under a matching key, and the lineage-end callback is
// told, outside the lock, each time such a handle ends; a handle without a lineage tells nothing.
func TestLineageEndTellsEachEndOfAHandleUnderALineage(t *testing.T) {
	f := newFixture(t, nil)
	key := func(k string) func(string) bool { return func(got string) bool { return got == k } }
	var seen []bool
	f.m.SetLineageEnd(func() {
		// Outside the manager's lock: a look back at it does not wait.
		seen = append(seen, f.m.HasLiveLineage(key("run#1")))
	})
	for _, reg := range []Registration{
		{ID: "a", Owner: "s", Kind: "fake", Lineage: []string{"caller", "run#1"}},
		{ID: "b", Owner: "s", Kind: "fake", Lineage: []string{"run#1"}},
		{ID: "c", Owner: "s", Kind: "fake"},
		{ID: "d", Owner: "s", Kind: "fake", Lineage: []string{"run#2"}},
	} {
		if err := f.m.Register(reg); err != nil {
			t.Fatal(err)
		}
	}
	if !f.m.HasLiveLineage(key("run#1")) || !f.m.HasLiveLineage(key("caller")) || f.m.HasLiveLineage(key("run#3")) {
		t.Fatal("HasLiveLineage does not match the live handles' lineage")
	}
	f.finish(t, "c", Result{Summary: "no lineage"})
	if len(seen) != 0 {
		t.Fatalf("a handle without a lineage told the callback: %v", seen)
	}
	f.finish(t, "a", Result{Summary: "one of two"})
	f.finish(t, "b", Result{Summary: "the last"})
	if !reflect.DeepEqual(seen, []bool{true, false}) {
		t.Fatalf("callback saw live = %v, want [true false]", seen)
	}
	if f.m.HasLiveLineage(key("run#1")) || !f.m.HasLiveLineage(key("run#2")) {
		t.Fatal("ended handles still count, or a live one under another key does not")
	}
}
