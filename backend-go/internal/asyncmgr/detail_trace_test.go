package asyncmgr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompleteKeepsDetailForStatusAndTraceOutOfEveryEntry(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SummaryMaxBytes, c.StatusSummaryBytes = 16, 64 })
	f.start(t, "s1", "a")
	long := strings.Repeat("x", 100)
	f.finish(t, "a", Result{Summary: long, Detail: long, Trace: "turn-7"})

	status, err := f.m.Status("s1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Summary) != 64 || !status.Cut {
		t.Fatalf("status summary = %d bytes cut=%v, want the detail cut to status_summary_max_bytes", len(status.Summary), status.Cut)
	}
	wait, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(wait.Signals) != 1 || len(wait.Signals[0].Summary) != 16 {
		t.Fatalf("wait = %+v, %v; want the 16-byte summary", wait, err)
	}
	for _, entry := range []Entry{status, wait.Signals[0]} {
		raw, _ := json.Marshal(entry)
		if strings.Contains(string(raw), "turn-7") {
			t.Fatalf("the trace reached an entry: %s", raw)
		}
	}
	rec, err := f.m.readRecord("a")
	if err != nil || rec.Trace != "turn-7" || len(rec.Detail) != 64 {
		t.Fatalf("record = %+v, %v; want the trace and the cut detail kept", rec, err)
	}
}

func TestStatusPrefersTheDriverDetailOverTheRecordDetail(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: "short", Detail: "from the record"})
	status, err := f.m.Status("s1", "a")
	if err != nil || status.Summary != "from the record" {
		t.Fatalf("status = %+v, %v", status, err)
	}
}
