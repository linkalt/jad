package asyncmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCompleteKeepsDetailForStatusAndTraceOutOfEveryEntry(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	long := strings.Repeat("x", 3000)
	f.finish(t, "a", Result{Summary: long, Detail: long, Trace: "turn-7"})

	status, err := f.m.Status("s1", "a")
	if err != nil || len(status.Summary) != 3000 || status.Cut {
		t.Fatalf("status summary = %d bytes, %v; want the whole result", len(status.Summary), err)
	}
	wait, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(wait.Signals) != 1 || len(wait.Signals[0].Summary) != 3000 {
		t.Fatalf("wait = %+v, %v; want the whole result", wait, err)
	}
	for _, entry := range []Entry{status, wait.Signals[0]} {
		raw, _ := json.Marshal(entry)
		if strings.Contains(string(raw), "turn-7") {
			t.Fatalf("the trace reached an entry: %s", raw)
		}
	}
	rec, err := f.m.readRecord("a")
	if err != nil || rec.Trace != "turn-7" || len(rec.Detail) != 3000 {
		t.Fatalf("record = %+v, %v; want the trace and the detail kept", rec, err)
	}
	if len(rec.Summary) > shortSummaryBytes {
		t.Fatalf("the record keeps %d summary bytes beside the detail, want at most %d", len(rec.Summary), shortSummaryBytes)
	}
	raw, _ := os.ReadFile(f.m.recordPath("a"))
	if len(raw) > 3000+1024 {
		t.Fatalf("record file is %d bytes, want the text stored once", len(raw))
	}
}

func TestResultNearTheCapIsDeliveredTheSameByWaitAndStatus(t *testing.T) {
	f := newFixture(t, nil)
	limit := f.m.detailLimit()
	for _, size := range []int{limit, limit + 500} {
		id := fmt.Sprintf("n%d", size)
		f.start(t, "s1", id)
		f.finish(t, id, Result{Summary: strings.Repeat("y", size), Detail: strings.Repeat("y", size)})
		status, err := f.m.Status("s1", id)
		if err != nil {
			t.Fatal(err)
		}
		wait, err := f.m.Wait(background, "s1", []string{id}, 0)
		if err != nil || len(wait.Signals) != 1 {
			t.Fatalf("wait = %+v, %v", wait, err)
		}
		got := wait.Signals[0]
		if len(got.Summary) != limit || len(status.Summary) != limit {
			t.Fatalf("size %d: wait %d bytes, status %d bytes, want %d for both", size, len(got.Summary), len(status.Summary), limit)
		}
		if got.Cut != (size > limit) || status.Cut != got.Cut {
			t.Fatalf("size %d: cut wait=%v status=%v", size, got.Cut, status.Cut)
		}
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
