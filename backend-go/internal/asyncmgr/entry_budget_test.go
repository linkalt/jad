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
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{
		Code: strings.Repeat("c", codeMaxBytes), Reason: strings.Repeat("r", reasonMaxBytes),
		Summary: strings.Repeat("s", 512), Ref: strings.Repeat("f", refMaxBytes),
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
}
