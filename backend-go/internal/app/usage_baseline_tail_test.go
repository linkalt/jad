package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A crash can leave bytes after the last newline of messages.jsonl (a torn append, or a hole of
// zero bytes). The backend never writes an empty line, so these are the tails it can meet. Both
// compactions repair the tail by reading first, and the totals and the next message are right.
func TestCompactionsAfterATornTailKeepTheTotals(t *testing.T) {
	tails := map[string]string{
		"torn record":     `{"type":"response.completed","session_turn":5,"response":{"id":"resp-x","usage":{"input_`,
		"torn first line": `{"type":"mes`,
		"zero bytes":      "\x00\x00\x00\x00",
	}
	for tailName, tail := range tails {
		for _, action := range []string{"/compact", "/front-compact"} {
			t.Run(tailName+action, func(t *testing.T) {
				f := newForkUsageFixture(t)
				f.runs("local:demo", "dev/coder", 1, 4)
				ids := f.userIDs("local:demo", "dev/coder")
				path := filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl")
				appendToTranscript(t, path, tail)
				body := ""
				if action == "/front-compact" {
					body = `{"message_id":"` + ids[2] + `"}`
				}
				if status, out := f.post("local:demo", "dev/coder", action, body); status != http.StatusOK {
					t.Fatalf("%s: %d %s", action, status, out)
				}
				if data, err := os.ReadFile(path); err != nil || (len(data) > 0 && data[len(data)-1] != '\n') {
					t.Fatalf("transcript after %s ends without a newline: %q (%v)", action, data, err)
				}
				removeFile(t, f.cachePath("demo", "dev/coder"))
				if usage := f.usageOf("local:demo", "dev/coder"); usage.HistoryPartial {
					t.Fatalf("usage after %s = %+v, want a complete history", action, usage)
				} else {
					wantResponses(t, "after "+action, usage, 4)
				}
				f.run("local:demo", "dev/coder", "turn 5")
				wantResponses(t, "after the next message", f.usageOf("local:demo", "dev/coder"), 5)
			})
		}
	}
}

// Empty lines are not something the backend writes (see the transcript package's test), so only an
// edit from outside can leave them. A compaction accepts them; a front compaction refuses and
// changes nothing. This pins that refusal as a known limit: if it is ever lifted, this test is
// what to update, together with DECISIONS.md.
func TestFrontCompactionRefusesATranscriptEndingInEmptyLinesAndChangesNothing(t *testing.T) {
	f := newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 4)
	ids := f.userIDs("local:demo", "dev/coder")
	path := filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl")
	appendToTranscript(t, path, "\n\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, out := f.post("local:demo", "dev/coder", "/front-compact", `{"message_id":"`+ids[2]+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("front-compact: %d %s, want 409", status, out)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("a refused front compaction changed the transcript (%v)", err)
	}
	if _, err := os.Stat(f.baselinePath("demo", "dev/coder")); !os.IsNotExist(err) {
		t.Fatalf("a refused front compaction left a baseline: %v", err)
	}
	wantResponses(t, "after the refusal", f.usageOf("local:demo", "dev/coder"), 4)
	f.run("local:demo", "dev/coder", "turn 5")
	wantResponses(t, "after the next message", f.usageOf("local:demo", "dev/coder"), 5)
}

// Where the history block is the only thing that says history was lost, losing it too leaves the
// totals reduced with nothing to say so. This pins both sides: with the history block the loss is
// flagged, without it it is not (nothing else records that a compaction happened).
func TestLostTotalsAreFlaggedOnlyWhileTheHistoryBlockExists(t *testing.T) {
	f := newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 3)
	if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
		t.Fatalf("compact: %d %s", status, out)
	}
	f.run("local:demo", "dev/coder", "turn 4")
	wantResponses(t, "before the loss", f.usageOf("local:demo", "dev/coder"), 4)

	// The cache and the baseline are gone (a restored or hand-cleaned folder).
	removeFile(t, f.cachePath("demo", "dev/coder"))
	removeFile(t, f.baselinePath("demo", "dev/coder"))
	if usage := f.usageOf("local:demo", "dev/coder"); !usage.HistoryPartial || usage.CompletedResponses != 1 {
		t.Fatalf("with the history block = %+v, want the one kept response and a partial history", usage)
	}

	// And the history block too: the totals are the same reduced ones, and nothing flags them.
	removeFile(t, f.cachePath("demo", "dev/coder"))
	removeFile(t, filepath.Join(f.stateDir("demo", "dev/coder"), "messages.backup.jsonl"))
	if usage := f.usageOf("local:demo", "dev/coder"); usage.HistoryPartial || usage.CompletedResponses != 1 {
		t.Fatalf("without the history block = %+v, want one response and no flag (a known gap)", usage)
	}
}

// A reader that meets a torn tail first (a list or snapshot does not repair it) counts the complete
// records, and the next message, which does repair it, adds to the same totals; with the cache
// gone the scan gives the same numbers.
func TestReaderOfATornTailAndTheNextMessageAgree(t *testing.T) {
	f := newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 3)
	path := filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl")
	appendToTranscript(t, path, `{"type":"response.completed","session_turn":4,"response":{"id":"torn`)
	wantResponses(t, "reader of the torn tail", f.usageOf("local:demo", "dev/coder"), 3)
	f.run("local:demo", "dev/coder", "turn 4")
	wantResponses(t, "after the next message", f.usageOf("local:demo", "dev/coder"), 4)
	removeFile(t, f.cachePath("demo", "dev/coder"))
	wantResponses(t, "after the next message, no cache", f.usageOf("local:demo", "dev/coder"), 4)
}
