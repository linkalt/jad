package asyncmgr

import "testing"

func TestMarkReadConsumesTheSignalOfTheOwnerOnly(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.start(t, "s1", "b")
	f.finish(t, "a", Result{})
	f.finish(t, "b", Result{})
	f.m.MarkRead("s2", "a")
	f.m.MarkRead("s1", "a")
	f.m.MarkRead("s1", "missing")
	res, err := f.m.Wait(t.Context(), "s1", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Signals); len(got) != 1 || got[0] != "b" {
		t.Fatalf("signals = %v, want only b", got)
	}
}

func TestCompleteKeepsTheDriversCutFlag(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{State: StateDone, OK: true, Summary: "tail", Cut: true})
	entry, err := f.m.Status("s1", "a")
	if err != nil || !entry.Cut {
		t.Fatalf("entry = %+v, err = %v, want cut", entry, err)
	}
}

func TestConfigRejectsNegativeStopWait(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StopWaitMs = -1
	if cfg.Validate() == nil {
		t.Fatal("negative stop_wait_ms accepted")
	}
}
