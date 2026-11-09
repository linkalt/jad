package asyncmgr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestRefTranscriptReachesWaitStatusAndTheRecordButNotBeyondItsCap: ref_transcript is a second,
// separate path beside ref in every entry and in the record.
func TestRefTranscriptReachesWaitStatusAndTheRecordButNotBeyondItsCap(t *testing.T) {
	f := newFixture(t, nil)
	f.start(t, "s1", "a")
	f.finish(t, "a", Result{Summary: "ok", Detail: "ok", Ref: "/state/a.txt", RefTranscript: "/t/" + strings.Repeat("x", 2*refMaxBytes), Bytes: 2})
	status, err := f.m.Status("s1", "a")
	if err != nil || status.Ref != "/state/a.txt" || len(status.RefTranscript) != refMaxBytes || !strings.HasPrefix(status.RefTranscript, "/t/") {
		t.Fatalf("status = %+v, %v", status, err)
	}
	wait, err := f.m.Wait(background, "s1", nil, 0)
	if err != nil || len(wait.Signals) != 1 || wait.Signals[0] != status {
		t.Fatalf("wait = %+v, %v; want the status entry", wait, err)
	}
	raw, _ := json.Marshal(status)
	if !strings.Contains(string(raw), `"ref":"/state/a.txt","ref_transcript":"/t/`) {
		t.Fatalf("entry field order: %s", raw)
	}
	rec, err := f.m.readRecord("a")
	if err != nil || rec.RefTranscript != status.RefTranscript {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

// TestDetailEntryIsCutOnlyWhenTheResultIs: the short summary kept beside a detail is cut from
// the detail, which says nothing about the result. The entry reports the result's cut: the
// driver's, the record's detail limit, or none.
func TestDetailEntryIsCutOnlyWhenTheResultIs(t *testing.T) {
	f := newFixture(t, nil)
	text := strings.Repeat("y", 3000) // over summary_max_bytes, within the detail limit
	f.start(t, "s1", "whole")
	f.finish(t, "whole", Result{Summary: text, Detail: text, Bytes: 3000})
	f.start(t, "s1", "src")
	f.finish(t, "src", Result{Summary: text, Detail: text, Cut: true, Bytes: 9000})
	f.start(t, "s1", "big")
	f.finish(t, "big", Result{Summary: text, Detail: strings.Repeat("z", 20_000), Bytes: 20_000})
	want := map[string]bool{"whole": false, "src": true, "big": true}
	for id, cut := range want {
		status, err := f.m.Status("s1", id)
		if err != nil || status.Cut != cut {
			t.Errorf("%s status cut = %v (%v), want %v", id, status.Cut, err, cut)
		}
	}
	for id, cut := range want {
		wait, err := f.m.Wait(background, "s1", []string{id}, 0)
		if err != nil || len(wait.Signals) != 1 || wait.Signals[0].Cut != cut {
			t.Errorf("%s wait = %+v (%v), want one entry with cut %v", id, wait.Signals, err, cut)
		}
	}
	info, err := os.Stat(f.m.recordPath("big"))
	if err != nil || info.Size() > int64(f.m.detailLimit())+2048 {
		t.Fatalf("record of a big result: %v %v, want it bounded by the detail limit", info, err)
	}
}

// TestFitEntryDropsTheTranscriptRefBeforeShrinkingTheSummary: under a tight budget the pointer
// to the transcript goes after the resume fields and before any of the summary.
func TestFitEntryDropsTheTranscriptRefBeforeShrinkingTheSummary(t *testing.T) {
	e := Entry{ID: "a", Kind: "k", State: StateDone, Summary: strings.Repeat("s", 100), Ref: "/r.txt", RefTranscript: strings.Repeat("t", 300)}
	got := fitEntry(e, encodedSize(e)-50)
	if got.RefTranscript != "" || got.Summary != e.Summary || got.Ref != e.Ref || !got.Cut {
		t.Fatalf("fitEntry = %+v; want ref_transcript dropped, summary and ref kept, cut", got)
	}
}
