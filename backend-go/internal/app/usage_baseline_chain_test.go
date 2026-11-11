package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
)

// frontCompact keeps the first turns user turns of a baselineTurns transcript.
func (b baselineSession) frontCompact(t *testing.T, turns int) {
	t.Helper()
	messageID := fmt.Sprintf("session-user-o%d", len(baselineTurns(1, turns)))
	if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "permanent"); err != nil {
		t.Fatalf("front compaction keeping %d turns: %v", turns, err)
	}
	if got := b.transcriptText(t); got != baselineTurns(1, turns) {
		t.Fatalf("front compaction kept %q", got)
	}
}

// A rewrite that fails to see a record it replaces must be refused, but empty lines after the last
// record are bytes the usage scan skips and could not have counted. A transcript with one used to
// make every compaction fail for good (the scan's end never matched the file's size).
func TestEmptyLinesAfterTheLastRecordDoNotBlockARewrite(t *testing.T) {
	t.Run("compaction", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.appendRaw(t, "\n\n")
		before := b.scan(t)
		if before.through >= before.size {
			t.Fatalf("the scan ends at %d of %d bytes; the test needs blank lines after it", before.through, before.size)
		}
		b.compact(t)
		removeUsageCache(t, b.config)
		wantTotals(t, "after the compaction, no cache", b.scan(t).usage, 4)
	})
	t.Run("a record after the blank lines is still refused", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.appendRaw(t, "\n")
		grown := false
		err := b.shared.rewritePreservingUsage(b.config, "test", false, func(before func(transcript.Replacement) error) (bool, error) {
			if !grown {
				grown = true
				b.appendRaw(t, "\n"+baselineTurns(5, 1))
			}
			options := baselineCompaction
			options.BeforePublish = before
			result, err := b.shared.store.CompactHistory(options)
			return result.Compacted, err
		})
		if err == nil || b.transcriptText(t) == "" {
			t.Fatalf("a rewrite that missed a record succeeded or emptied the transcript: %v", err)
		}
		wantTotals(t, "after the refused rewrite", b.scan(t).usage, 5)
	})
}

// A front compaction says whether anything was removed from before the bytes it keeps, and keeps
// every older entry that still fits them; an ordinary compaction removes history from before.
func TestFrontCompactionEntriesSayWhatWasRemovedBeforeThePrefix(t *testing.T) {
	t.Run("a session never compacted", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.frontCompact(t, 2)
		entries := baselineEntries(t, b.config)
		if len(entries) != 1 || !entries[0].PrefixExact {
			t.Fatalf("entries = %+v, want one that says nothing was removed before the prefix", entries)
		}
	})
	t.Run("ordinary compaction", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.compact(t)
		if entries := baselineEntries(t, b.config); len(entries) != 1 || entries[0].PrefixExact {
			t.Fatalf("entries = %+v, want one entry that is not prefix-exact", entries)
		}
	})
	t.Run("ordinary then front keeps both checkpoints", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.compact(t)
		b.appendRaw(t, baselineTurns(5, 4))
		b.scan(t)
		// The kept prefix is the two turns after the compaction; its offset is their size.
		messageID := fmt.Sprintf("session-user-o%d", len(baselineTurns(5, 2)))
		if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "permanent"); err != nil {
			t.Fatal(err)
		}
		entries := baselineEntries(t, b.config)
		if len(entries) != 2 || entries[0].PrefixExact || entries[0].Offset != int64(len(baselineTurns(5, 2))) || entries[1].Offset != 0 || entries[1].Summary.CompletedResponses != 4 {
			t.Fatalf("entries = %+v, want the front entry (not exact) and the earlier one at offset 0 with 4 responses", entries)
		}
		removeUsageCache(t, b.config)
		wantTotals(t, "after both, no cache", b.scan(t).usage, 8)
	})
	t.Run("front twice keeps the older checkpoint and stays exact", func(t *testing.T) {
		b := newBaselineSession(t, 5)
		b.scan(t)
		b.frontCompact(t, 3)
		// Turns that differ from the ones the first compaction removed, or the file would begin with
		// the bytes that entry replaced and the entry would not fit it.
		b.appendRaw(t, baselineTurns(10, 3))
		b.scan(t)
		messageID := fmt.Sprintf("session-user-o%d", len(baselineTurns(1, 3))+len(baselineTurns(10, 2)))
		if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "permanent"); err != nil {
			t.Fatal(err)
		}
		entries := baselineEntries(t, b.config)
		if len(entries) != 2 || !entries[0].PrefixExact || !entries[1].PrefixExact || entries[1].Offset != int64(len(baselineTurns(1, 3))) || entries[0].Offset <= entries[1].Offset {
			t.Fatalf("entries = %+v, want two exact checkpoints, the newer at the larger offset", entries)
		}
		removeUsageCache(t, b.config)
		wantTotals(t, "after both, no cache", b.scan(t).usage, 8)
	})
	t.Run("a front compaction that replaces a smaller offset keeps one entry per offset", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.frontCompact(t, 3)
		b.frontCompact(t, 2)
		entries := baselineEntries(t, b.config)
		if len(entries) != 2 {
			t.Fatalf("entries = %+v, want the new one and the crash-window one", entries)
		}
		seen := map[int64]bool{}
		for _, entry := range entries {
			if seen[entry.Offset] {
				t.Fatalf("two entries at offset %d: %+v", entry.Offset, entries)
			}
			seen[entry.Offset] = true
		}
	})
	t.Run("every older entry that fits the kept prefix stays", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.compact(t)
		b.appendRaw(t, baselineTurns(5, 6))
		b.scan(t)
		first := fmt.Sprintf("session-user-o%d", len(baselineTurns(5, 4)))
		if _, err := b.shared.frontCompactPreservingUsage(b.config, first, "permanent"); err != nil {
			t.Fatal(err)
		}
		// A smaller prefix: the first front entry no longer fits it, the compaction's does.
		second := fmt.Sprintf("session-user-o%d", len(baselineTurns(5, 2)))
		if _, err := b.shared.frontCompactPreservingUsage(b.config, second, "permanent"); err != nil {
			t.Fatal(err)
		}
		entries := baselineEntries(t, b.config)
		if len(entries) != 3 || entries[0].Offset != int64(len(baselineTurns(5, 2))) || entries[1].Offset != int64(len(baselineTurns(5, 4))) || entries[2].Offset != 0 {
			t.Fatalf("entries = %+v, want the new one, the one that was current, and the compaction's at offset 0", entries)
		}
	})
	t.Run("an unreadable baseline proves nothing", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		if err := os.WriteFile(sessionfs.UsageBaselinePath(b.config.UsageCachePath), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		b.frontCompact(t, 2)
		if entries := baselineEntries(t, b.config); len(entries) != 1 || entries[0].PrefixExact {
			t.Fatalf("entries = %+v, want one entry that is not prefix-exact", entries)
		}
	})
	t.Run("a partial history proves nothing", func(t *testing.T) {
		b := newBaselineSession(t, 4)
		b.scan(t)
		b.compact(t)
		b.appendRaw(t, baselineTurns(5, 2))
		// The baseline and cache are lost: what the compaction removed is unknown.
		for _, path := range []string{sessionfs.UsageBaselinePath(b.config.UsageCachePath), b.config.UsageCachePath} {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if !b.scan(t).usage.HistoryPartial {
			t.Fatal("the session is not partial; the test needs a lost baseline")
		}
		messageID := fmt.Sprintf("session-user-o%d", len(baselineTurns(5, 1)))
		if _, err := b.shared.frontCompactPreservingUsage(b.config, messageID, "permanent"); err != nil {
			t.Fatal(err)
		}
		entries := baselineEntries(t, b.config)
		if len(entries) != 1 || entries[0].PrefixExact || !entries[0].Summary.HistoryPartial {
			t.Fatalf("entries = %+v, want one partial entry that is not prefix-exact", entries)
		}
	})
}

// What a fork that copies the first end bytes carries, for a source after an ordinary compaction
// that kept the preserved response (the records it keeps hold no turn start, so a cut inside them
// is not a fork point; the decision is still made, and says partial).
func TestForkUsageDecision(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	if got := b.shared.forkUsageBaseline(b.config, 100, false); !got.none() {
		t.Fatalf("a source with no entry and complete history: %+v, want nothing", got)
	}
	if got := b.shared.forkUsageBaseline(b.config, 100, true); !got.partial {
		t.Fatalf("a source with no entry and partial history: %+v, want a partial marker", got)
	}
	options := baselineCompaction
	options.PreserveResponseID = "resp-4"
	if _, err := b.shared.compactPreservingUsage(b.config, options); err != nil {
		t.Fatal(err)
	}
	size := int64(len(b.transcriptText(t)))
	entries := baselineEntries(t, b.config)
	if size == 0 || len(entries) != 1 || entries[0].Offset != size {
		t.Fatalf("the compaction kept %d bytes with entries %+v; the test needs the preserved response kept", size, entries)
	}
	if got := b.shared.forkUsageBaseline(b.config, size, false); got.entry == nil || got.entry.Offset != size {
		t.Fatalf("a fork of the whole file: %+v, want the entry", got)
	}
	if got := b.shared.forkUsageBaseline(b.config, size-1, false); !got.partial || got.entry != nil || got.exact {
		t.Fatalf("a fork cutting inside the kept response: %+v, want a partial marker", got)
	}
	if got := b.shared.forkUsageBaseline(b.config, 0, true); !got.none() {
		t.Fatalf("a fork that copies nothing: %+v, want nothing", got)
	}
}

// The entry is bound to the fork's bytes only after the source bytes it covers are proven to be
// the ones it hashes; otherwise the fork says its history is partial, in its own baseline file.
func TestForkBaselineBindsTheEntryOnlyToProvenBytes(t *testing.T) {
	b := newBaselineSession(t, 3)
	b.scan(t)
	b.frontCompact(t, 2)
	entry := baselineEntries(t, b.config)[0]
	lines := []string{}
	for _, line := range splitLines(baselineTurns(1, 2)) {
		lines = append(lines, line)
	}
	config := b.config
	config.UsageCachePath = t.TempDir() + "/usage-cache.json"
	run := func(entry usageBaselineEntry, gapBefore int64, tamper bool) []usageBaselineEntry {
		path := sessionfs.UsageBaselinePath(config.UsageCachePath)
		_ = os.Remove(path)
		fork := newForkBaseline(forkUsage{entry: &entry})
		offset := gapBefore
		for index, line := range lines {
			text := line
			if tamper && index == 0 {
				// The same length, so only the hash can tell.
				text = strings.Replace(text, "question", "Question", 1)
			}
			fork.add(offset, []byte(text))
			offset += int64(len(text)) + 1
		}
		if _, err := fork.store(config, nil, offset); err != nil {
			t.Fatal(err)
		}
		file, err := readUsageBaselineFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return file.Entries
	}
	if got := run(entry, 0, false); len(got) != 1 || got[0].Offset != entry.Offset || got[0].SHA256 != entry.SHA256 || got[0].Summary.HistoryPartial {
		t.Fatalf("proven bytes: %+v, want the entry itself", got)
	}
	if got := run(entry, 0, true); len(got) != 1 || got[0].Offset != 0 || !got[0].Summary.HistoryPartial || got[0].Summary.CompletedResponses != 0 {
		t.Fatalf("changed bytes: %+v, want a partial marker", got)
	}
	// A different file's offsets: a gap the entry's hash does not cover.
	if got := run(entry, 1, false); len(got) != 1 || !got[0].Summary.HistoryPartial {
		t.Fatalf("a leading gap: %+v, want a partial marker", got)
	}
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index])
			start = index + 1
		}
	}
	return lines
}

func marshalBaseline(entries []usageBaselineEntry) ([]byte, error) {
	return json.Marshal(usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: entries})
}

// CompactHistory asks the hook twice: before it writes the backup, and right before the rename. A
// record appended between the two makes the second ask refuse, after the backup and the compaction
// cache were written. The transcript is then still the old file with its new record, the entry the
// first ask stored does not fit it (it names the file it replaced), and no total is lost or
// counted twice; a retry goes through.
func TestRecordAppendedBetweenTheTwoAsksOfTheHookLosesNothing(t *testing.T) {
	b := newBaselineSession(t, 4)
	b.scan(t)
	before := b.scan(t)
	var seen transcript.Replacement
	store := b.shared.usageBaselineHook(b.config, before, &seen, false)
	options := baselineCompaction
	calls := 0
	options.BeforePublish = func(replacement transcript.Replacement) error {
		calls++
		if err := store(replacement); err != nil {
			return err
		}
		if calls == 1 {
			b.appendRaw(t, baselineTurns(5, 1))
		}
		return nil
	}
	// The hook is asked the second time about a transcript that grew: PreviousSize differs from
	// the scan's end, so the stale check refuses.
	_, err := b.shared.store.CompactHistory(options)
	if err == nil || calls != 2 {
		t.Fatalf("compaction: error %v after %d asks, want a refusal at the second", err, calls)
	}
	if !strings.Contains(b.transcriptText(t), `"resp-5"`) || !strings.Contains(b.transcriptText(t), `"resp-1"`) {
		t.Fatal("the refused compaction changed the transcript")
	}
	removeUsageCache(t, b.config)
	wantTotals(t, "old transcript, entry stored", b.scan(t).usage, 5)
	b.compact(t)
	removeUsageCache(t, b.config)
	wantTotals(t, "after the retry", b.scan(t).usage, 5)
}
