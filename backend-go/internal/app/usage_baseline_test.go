package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
)

func removeUsageCache(t *testing.T, config sessionfs.SessionConfig) {
	t.Helper()
	if err := os.Remove(config.UsageCachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func (b baselineSession) compact(t *testing.T) {
	t.Helper()
	result, err := b.shared.compactPreservingUsage(b.config, baselineCompaction)
	if err != nil || !result.Compacted {
		t.Fatalf("compaction: %+v, %v", result, err)
	}
}

func baselineEntries(t *testing.T, config sessionfs.SessionConfig) []usageBaselineEntry {
	t.Helper()
	file, err := readUsageBaselineFile(sessionfs.UsageBaselinePath(config.UsageCachePath))
	if err != nil {
		t.Fatal(err)
	}
	return file.Entries
}

// Compaction removes records from the transcript, so their usage must survive in something that
// is not the cache: a reader that finds no cache (a compaction in flight, a failed cache write,
// a restored session folder) rebuilds from the kept records and would otherwise report less.
func TestCompactionKeepsTotalsWhenTheUsageCacheIsGone(t *testing.T) {
	b := newBaselineSession(t, 4)
	wantTotals(t, "before", b.scan(t).usage, 4)
	full := b.transcriptText(t)

	b.compact(t)
	if kept := b.transcriptText(t); len(kept) >= len(full) {
		t.Fatalf("compaction kept %d of %d bytes; the test needs it to remove records", len(kept), len(full))
	}
	removeUsageCache(t, b.config)

	after := b.scan(t)
	wantTotals(t, "after compaction, no cache", after.usage, 4)
	if after.usage.TurnCount != 4 || after.usage.HistoryPartial {
		t.Fatalf("turn count %d, partial %v; want 4 and false", after.usage.TurnCount, after.usage.HistoryPartial)
	}
	current := after.usage.Current
	if current == nil || current.InputTokens == nil || *current.InputTokens != 0 || current.ResponseID != "resp-4" {
		t.Fatalf("current = %+v, want resp-4 with its tokens reset to 0", current)
	}
	if !usageCacheExists(t, b.config) {
		t.Fatal("the scan from the baseline did not republish the cache")
	}
}

// A reader that runs between the transcript rewrite and the cache republish, the window the
// transcript package leaves by deleting the cache, sees the same totals as before.
func TestReaderInTheGapAfterThePublishSeesTheTotals(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	var gap transcriptIndex
	var gapErr error
	afterRewritePublished = func() {
		if usageCacheExists(t, b.config) {
			t.Error("the usage cache exists in the gap; the test does not cover it")
		}
		gap, gapErr = scanTranscriptIndex(b.shared, b.config)
	}
	t.Cleanup(func() { afterRewritePublished = nil })

	b.compact(t)
	if gapErr != nil {
		t.Fatal(gapErr)
	}
	wantTotals(t, "reader in the gap", gap.usage, 4)
	// The cache the reader wrote and the one the compaction wrote describe the same totals.
	cache, ok := readUsageCacheState(b.config)
	if !ok {
		t.Fatal("no valid usage cache after the compaction")
	}
	wantTotals(t, "cache after the compaction", cache.Summary, 4)
}

// A message after a compaction adds to the kept totals, with the cache present and without it.
func TestNextMessageAfterCompactionAccumulates(t *testing.T) {
	for _, dropCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("dropCache=%v", dropCache), func(t *testing.T) {
			b := newBaselineSession(t, 4)
			b.scan(t)
			b.compact(t)
			if dropCache {
				removeUsageCache(t, b.config)
			}
			b.appendRaw(t, baselineTurns(5, 1))
			after := b.scan(t)
			wantTotals(t, "after the next message", after.usage, 5)
			if after.usage.TurnCount != 5 {
				t.Fatalf("turn count = %d, want 5, so the next run is turn 6", after.usage.TurnCount)
			}
			current := after.usage.Current
			if current == nil || current.ResponseID != "resp-5" || current.InputTokens == nil || *current.InputTokens != 10 {
				t.Fatalf("current = %+v, want resp-5 with 10 input tokens", current)
			}
		})
	}
}

// Every compaction in a series starts from the previous baseline, never from the kept records
// alone, whether or not a cache is there to hide the difference.
func TestRepeatedCompactionsKeepEveryTotal(t *testing.T) {
	for _, dropCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("dropCache=%v", dropCache), func(t *testing.T) {
			b := newBaselineSession(t, 3)
			total := int64(3)
			for round := 0; round < 5; round++ {
				if dropCache {
					removeUsageCache(t, b.config)
				}
				wantTotals(t, fmt.Sprintf("round %d before", round), b.scan(t).usage, total)
				b.compact(t)
				if dropCache {
					removeUsageCache(t, b.config)
				}
				wantTotals(t, fmt.Sprintf("round %d after", round), b.scan(t).usage, total)
				b.appendRaw(t, baselineTurns(int(total)+1, 2))
				total += 2
			}
			if entries := baselineEntries(t, b.config); len(entries) > 2 {
				t.Fatalf("baseline keeps %d entries, want at most 2", len(entries))
			}
		})
	}
}

// A compaction that keeps its newest turn whole keeps that turn's response in the transcript;
// the baseline covers it, so it is not counted again when the scan resumes.
func TestRetainedRecordsAreNotCountedTwice(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	options := baselineCompaction
	options.PreserveResponseID = "resp-4"
	if result, err := b.shared.compactPreservingUsage(b.config, options); err != nil || !result.Compacted {
		t.Fatalf("compaction: %+v, %v", result, err)
	}
	if !strings.Contains(b.transcriptText(t), `"resp-4"`) {
		t.Fatalf("the newest response was not kept: %q", b.transcriptText(t))
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "after", b.scan(t).usage, 4)
	removeUsageCache(t, b.config)
	b.appendRaw(t, baselineTurns(5, 1))
	wantTotals(t, "after the next message", b.scan(t).usage, 5)
}

// Records that are not responses (a state snapshot, a user message with no answer yet) change the
// turn count and nothing else, and do not disturb the baseline.
func TestNonRunAppendsAfterCompaction(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	b.compact(t)
	removeUsageCache(t, b.config)
	b.appendRaw(t, `{"type":"message","role":"system","content":[{"type":"input_text","text":"state"}]}`+"\n")
	b.appendRaw(t, `{"type":"message","role":"user","content":[{"type":"input_text","text":"unanswered"}]}`+"\n")
	after := b.scan(t)
	wantTotals(t, "after non-run appends", after.usage, 4)
	if after.usage.TurnCount != 5 {
		t.Fatalf("turn count = %d, want 5", after.usage.TurnCount)
	}
}

// Reset is the one operation that ends the totals, and it ends the baseline with them.
func TestResetEndsTheBaseline(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	b.compact(t)
	if len(baselineEntries(t, b.config)) == 0 {
		t.Fatal("no baseline to reset")
	}
	if err := b.shared.store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionfs.UsageBaselinePath(b.config.UsageCachePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("baseline after reset: %v", err)
	}
	if usage := b.scan(t).usage; usage.CompletedResponses != 0 || usage.TurnCount != 0 {
		t.Fatalf("usage after reset = %+v", usage)
	}
	b.appendRaw(t, baselineTurns(1, 1))
	wantTotals(t, "first message after reset", b.scan(t).usage, 1)
}

// A baseline that cannot be stored fails the compaction before the transcript changes: those
// totals could not be rebuilt, so nothing is removed without them.
func TestCompactionFailsWithoutChangingTheTranscriptWhenTheBaselineCannotBeStored(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	before := b.transcriptText(t)
	if err := os.MkdirAll(sessionfs.UsageBaselinePath(b.config.UsageCachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := b.shared.compactPreservingUsage(b.config, baselineCompaction); err == nil {
		t.Fatal("compaction succeeded although its baseline could not be stored")
	}
	if after := b.transcriptText(t); after != before {
		t.Fatalf("transcript changed by a failed compaction:\n%q\nwant\n%q", after, before)
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "after the failed compaction", b.scan(t).usage, 4)
}

// A failure between storing the baseline and publishing the transcript, or a crash there, leaves
// the old transcript current. Its totals must not be counted twice (once in the new entry, once
// from the records still in the file), on a first compaction or a later one.
func TestFailedPublishAfterTheBaselineIsStoredDoesNotDoubleCount(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(fmt.Sprintf("earlierCompaction=%v", earlier), func(t *testing.T) {
			b := newBaselineSession(t, 4)
			b.scan(t)
			total := int64(4)
			if earlier {
				b.compact(t)
				b.appendRaw(t, baselineTurns(5, 2))
				total = 6
			}
			wantTotals(t, "before", b.scan(t).usage, total)
			unchanged := b.transcriptText(t)

			before := b.scan(t)
			var seen transcript.Replacement
			store := b.shared.usageBaselineHook(b.config, before, &seen)
			options := baselineCompaction
			calls := 0
			options.BeforePublish = func(replacement transcript.Replacement) error {
				calls++
				if err := store(replacement); err != nil {
					return err
				}
				if calls == 2 { // the call right before the rename, after everything else was written
					return errors.New("injected failure before the rename")
				}
				return nil
			}
			if _, err := b.shared.store.CompactHistory(options); err == nil || calls != 2 {
				t.Fatalf("injected failure: error %v after %d hook calls, want a failure at the second", err, calls)
			}
			if b.transcriptText(t) != unchanged {
				t.Fatal("transcript changed by a compaction that failed before the rename")
			}
			if len(baselineEntries(t, b.config)) == 0 {
				t.Fatal("the new baseline entry was not stored; the test does not cover the window")
			}
			removeUsageCache(t, b.config)
			wantTotals(t, "old transcript, new entry stored", b.scan(t).usage, total)

			// The retry replaces the failed entry and keeps the earlier one.
			b.compact(t)
			removeUsageCache(t, b.config)
			wantTotals(t, "after the retry", b.scan(t).usage, total)
			if entries := baselineEntries(t, b.config); len(entries) > 2 {
				t.Fatalf("baseline keeps %d entries after a retry, want at most 2", len(entries))
			}
		})
	}
}

// A crash after the transcript is published and before the cache is republished: the cache is
// already deleted, so the baseline alone has to give the totals back.
func TestCrashAfterThePublishKeepsTheTotals(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	afterRewritePublished = func() { panic("simulated crash") }
	t.Cleanup(func() { afterRewritePublished = nil })
	func() {
		defer func() { _ = recover() }()
		b.compact(t)
	}()
	afterRewritePublished = nil
	if usageCacheExists(t, b.config) {
		t.Fatal("the cache exists after the simulated crash")
	}
	wantTotals(t, "after the crash", b.scan(t).usage, 4)
}

// The scan a rewrite starts from must account for every byte the rewrite replaces. A transcript
// that grew in between makes the rewrite refuse, with the transcript untouched.
func TestRewriteRefusesWhenTheTranscriptGrewAfterTheUsageScan(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	grown := false
	err := b.shared.rewritePreservingUsage(b.config, "test", func(before func(transcript.Replacement) error) (bool, error) {
		if !grown {
			grown = true
			b.appendRaw(t, baselineTurns(5, 1))
		}
		options := baselineCompaction
		options.BeforePublish = before
		result, err := b.shared.store.CompactHistory(options)
		return result.Compacted, err
	})
	if !errors.Is(err, errUsageBaselineStale) {
		t.Fatalf("rewrite error = %v, want errUsageBaselineStale", err)
	}
	if !strings.Contains(b.transcriptText(t), `"resp-5"`) || !strings.Contains(b.transcriptText(t), `"resp-1"`) {
		t.Fatal("a refused rewrite changed the transcript")
	}
	wantTotals(t, "after the refused rewrite", b.scan(t).usage, 5)
	// Retried, it goes through.
	b.compact(t)
	removeUsageCache(t, b.config)
	wantTotals(t, "after the retry", b.scan(t).usage, 5)
}

// Front compaction removes the transcript's tail, and its totals stay in the baseline the same
// way.
func TestFrontCompactionKeepsTotalsWhenTheUsageCacheIsGone(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	boundary := len(baselineTurns(1, 2))
	messageID := fmt.Sprintf("session-user-o%d", boundary)
	if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "permanent"); err != nil {
		t.Fatal(err)
	}
	if got := b.transcriptText(t); got != baselineTurns(1, 2) {
		t.Fatalf("front compaction kept %q", got)
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "after front compaction, no cache", b.scan(t).usage, 4)
	b.appendRaw(t, baselineTurns(5, 1))
	removeUsageCache(t, b.config)
	wantTotals(t, "after the next message", b.scan(t).usage, 5)
}

func TestFrontCompactionFailsWithoutChangingTheTranscriptWhenTheBaselineCannotBeStored(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	before := b.transcriptText(t)
	if err := os.MkdirAll(sessionfs.UsageBaselinePath(b.config.UsageCachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	messageID := fmt.Sprintf("session-user-o%d", len(baselineTurns(1, 2)))
	if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "recoverable"); err == nil {
		t.Fatal("front compaction succeeded although its baseline could not be stored")
	}
	if b.transcriptText(t) != before {
		t.Fatal("transcript changed by a failed front compaction")
	}
	if archives, _ := os.ReadDir(b.config.MessagesPath + ".deleted-history"); len(archives) != 0 {
		t.Fatalf("a failed front compaction left %d archives", len(archives))
	}
}

// Sessions from before the baseline have none; one that is unreadable is ignored and replaced by
// the next compaction.
func TestMissingAndCorruptBaselineFiles(t *testing.T) {
	b := newBaselineSession(t, 4)
	wantTotals(t, "no baseline file", b.scan(t).usage, 4)
	path := sessionfs.UsageBaselinePath(b.config.UsageCachePath)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "corrupt baseline file", b.scan(t).usage, 4)
	b.compact(t)
	removeUsageCache(t, b.config)
	wantTotals(t, "after replacing the corrupt baseline", b.scan(t).usage, 4)
}

// A session folder restored to another inode or copied elsewhere keeps its totals: the baseline
// is tied to the transcript's bytes, not to the file.
func TestBaselineSurvivesACopyOfTheTranscript(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	b.compact(t)
	b.appendRaw(t, baselineTurns(5, 1))
	data := b.transcriptText(t)
	if err := os.Remove(b.config.MessagesPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.config.MessagesPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	wantTotals(t, "after copying the transcript to a new file", b.scan(t).usage, 5)
}

// The scan that began on the transcript a compaction then replaced must not publish a cache for
// the new file made of the old file's totals.
func TestScanThatRacesACompactionDoesNotStampAStaleCache(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(fmt.Sprintf("seamReportsChange=%v", report), func(t *testing.T) {
			b := newBaselineSession(t, 4)
			b.scan(t)
			// Same-length turns put the old cache offset on a record boundary of the new file,
			// where scanning on from it is silently wrong rather than an error.
			raced := false
			installScanSnapshotRead(t, func(store *transcript.TurnStore, start, end int64, visit func(int64, []byte) error) error {
				if raced {
					return (*transcript.TurnStore).ReadRecordsSnapshot(store, start, end, visit)
				}
				raced = true
				if report {
					if err := (*transcript.TurnStore).ReadRecordsSnapshot(store, start, end, visit); err != nil {
						return err
					}
				}
				b.compact(t)
				b.appendRaw(t, baselineTurns(5, 5))
				if report {
					return transcript.ErrTranscriptChanged
				}
				return (*transcript.TurnStore).ReadRecordsSnapshot(store, start, end, visit)
			})
			// Add one turn to the old file so the scan has something to read: the cache is
			// valid for the file as it was, with its offset at the end of the 4 turns.
			index := b.scan(t)
			wantTotals(t, "racing scan", index.usage, 9)
			removeUsageCache(t, b.config)
			wantTotals(t, "scan after the race", b.scan(t).usage, 9)
		})
	}
}

// Readers that never take the run lock keep seeing at least the totals the session had, while
// compactions and messages rewrite and extend the transcript under them.
func TestConcurrentReadersDuringCompactionsAndMessages(t *testing.T) {
	b := newBaselineSession(t, 3)
	b.scan(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	failures := make(chan string, 64)
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				index, err := scanTranscriptIndex(b.shared, b.config)
				if errors.Is(err, transcript.ErrTranscriptChanged) {
					continue // a second rewrite inside one scan is reported, not served
				}
				if err != nil {
					failures <- err.Error()
					return
				}
				if index.usage.CompletedResponses < 3 {
					failures <- fmt.Sprintf("a reader saw %d completed responses, fewer than the session started with", index.usage.CompletedResponses)
					return
				}
			}
		}()
	}
	total := int64(3)
	for round := 0; round < 25; round++ {
		b.appendRaw(t, baselineTurns(int(total)+1, 1))
		total++
		b.compact(t)
	}
	close(stop)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "after the rounds", b.scan(t).usage, total)
}

// The same sequence through the HTTP routes with real runs: the totals survive a compaction
// whose cache is then lost, and the next message adds to them.
func TestHTTPNextMessageAfterCompactionAddsToTheKeptTotals(t *testing.T) {
	f := newLayoutFixture(t)
	f.usage = `"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6},`
	const session = "/api/sessions/local:demo"
	profile := "dev/coder"
	cachePath := filepath.Join(f.cwd, ".jad", "state", "demo", "dev", "coder", "staging", "usage-cache.json")
	run := func(text string) {
		t.Helper()
		body := fmt.Sprintf(`{"input":%q}`, text)
		if status, out := f.do(t, http.MethodPost, session+"/run?profile="+url.QueryEscape(profile), body); status != http.StatusOK {
			t.Fatalf("run: %d %s", status, out)
		}
	}
	completed := func() string {
		t.Helper()
		status, body := f.do(t, http.MethodGet, session+"?profile="+url.QueryEscape(profile), "")
		if status != http.StatusOK {
			t.Fatalf("read session: %d %s", status, body)
		}
		return extractUsage(body)
	}
	for turn := 1; turn <= 3; turn++ {
		run(fmt.Sprintf("turn %d", turn))
	}
	if usage := completed(); !strings.Contains(usage, `"completed_responses":3`) {
		t.Fatalf("usage before compaction: %s", usage)
	}
	if status, out := f.do(t, http.MethodPost, session+"/compact?profile="+url.QueryEscape(profile), ""); status != http.StatusOK {
		t.Fatalf("compact: %d %s", status, out)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	if usage := completed(); !strings.Contains(usage, `"completed_responses":3`) || !strings.Contains(usage, `"input_tokens":15`) {
		t.Fatalf("usage after compaction and a lost cache: %s", usage)
	}
	if err := os.Remove(cachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	run("turn 4")
	if usage := completed(); !strings.Contains(usage, `"completed_responses":4`) || !strings.Contains(usage, `"input_tokens":20`) {
		t.Fatalf("usage after the next message: %s", usage)
	}
}

// A cache is stamped with the identity of the file at the path when it is written, so a writer
// whose summary came from another file must not write it.
func TestUsageCacheIsNotStampedOnAFileItWasNotComputedFrom(t *testing.T) {
	b := newBaselineSession(t, 4)
	old := b.scan(t)
	removeUsageCache(t, b.config)
	b.compact(t)
	b.appendRaw(t, baselineTurns(5, 5))
	removeUsageCache(t, b.config)

	if b.shared.writeUsageCache(b.config, old.usage, old.through, old.lastResponseKey, old.generation) {
		t.Fatal("a cache computed from the replaced file was written")
	}
	if usageCacheExists(t, b.config) {
		t.Fatal("a stale cache file exists")
	}
	wantTotals(t, "after the refused write", b.scan(t).usage, 9)
}
