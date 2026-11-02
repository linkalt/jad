package asyncmgr

import (
	"strings"
	"testing"
)

// A worst-case entry with both remote-resume fields maxed must still fit the response budget
// that Validate guarantees, and wait and status must deliver it rather than defer it.
func TestWorstCaseEntryWithBothResumeFieldsFitsWaitAndStatus(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.SummaryMaxBytes = 512
		c.ResponseMaxBytes = 512 + EntryOverheadBytes
	})
	budget := int(f.m.cfg.ResponseMaxBytes) - envelopeBytes
	summary := strings.Repeat("s", 64)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{
		Code: strings.Repeat("c", codeMaxBytes), Reason: strings.Repeat("r", reasonMaxBytes),
		Summary: summary, Ref: strings.Repeat("f", refMaxBytes),
		ResumedFrom: strings.Repeat("p", refMaxBytes), ConfigNote: strings.Repeat("n", ConfigNoteMaxBytes),
	})

	status, err := f.m.Status("s1", "a")
	if err != nil || encodedSize(status) > budget {
		t.Fatalf("status = %d bytes, %v; want at most %d", encodedSize(status), err, budget)
	}
	wait, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(wait.Signals) != 1 {
		t.Fatalf("wait = %+v, %v; want the entry delivered, not deferred", wait, err)
	}
	if size := encodedSize(wait.Signals[0]); size > budget {
		t.Fatalf("wait entry = %d bytes, want at most %d", size, budget)
	}
	if e := wait.Signals[0]; e.ConfigNote != "" || e.ResumedFrom != "" || e.Summary != summary || !e.Cut {
		t.Fatalf("wait entry kept config_note=%d resumed_from=%d summary=%d cut=%v; want both dropped, summary whole, cut", len(e.ConfigNote), len(e.ResumedFrom), len(e.Summary), e.Cut)
	}
}

// A resumed_from that fits survives when only config_note has to go, and the entry says it was cut.
func TestOverBudgetEntryDropsConfigNoteFirstAndKeepsResumedFrom(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.SummaryMaxBytes = 512
		c.ResponseMaxBytes = 512 + EntryOverheadBytes
	})
	f.start(t, "s1", "a")
	summary := strings.Repeat("s", 400)
	f.finish(t, "a", Result{Summary: summary, ResumedFrom: strings.Repeat("p", 64), ConfigNote: strings.Repeat("n", ConfigNoteMaxBytes)})

	wait, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(wait.Signals) != 1 {
		t.Fatalf("wait = %+v, %v", wait, err)
	}
	e := wait.Signals[0]
	if e.ConfigNote != "" || e.ResumedFrom != strings.Repeat("p", 64) || e.Summary != summary || !e.Cut {
		t.Fatalf("wait entry config_note=%d resumed_from=%d cut=%v; want config_note dropped, resumed_from kept, cut", len(e.ConfigNote), len(e.ResumedFrom), e.Cut)
	}
}

// An entry that fits untouched keeps Cut clear, so non-resume entries stay byte-identical.
func TestFittingEntryIsNotMarkedCut(t *testing.T) {
	e := Entry{ID: "a", State: StateDone, ConfigNote: "note", ResumedFrom: "rr"}
	if got := fitEntry(e, 4096); got != e || got.Cut {
		t.Fatalf("fitEntry = %+v, want the entry unchanged and not cut", got)
	}
}
