package app

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
)

// installScanSnapshotRead replaces the scan's snapshot read for one test. Tests that call it
// must not run in parallel: the seam is a package variable.
func installScanSnapshotRead(t *testing.T, read func(store *transcript.TurnStore, start, end int64, visit func(int64, []byte) error) error) {
	t.Helper()
	previous := scanSnapshotRead
	scanSnapshotRead = read
	t.Cleanup(func() { scanSnapshotRead = previous })
}

// scanRetryFixture returns the shared store and config of a cold scan over a fixture transcript,
// with no usage cache written yet.
func scanRetryFixture(t *testing.T) (*sharedSession, sessionfs.SessionConfig) {
	t.Helper()
	resolver := newProfileTurnsBenchmarkResolver(t, 1, 50)
	location, err := sessionfs.Resolve(resolver.roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, "dev/p00", "")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := resolver.sharedStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.UsageCachePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return shared, config
}

func usageCacheExists(t *testing.T, config sessionfs.SessionConfig) bool {
	t.Helper()
	_, err := os.Stat(config.UsageCachePath)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}

// A first attempt that visits every record and then reports a change must be discarded in full:
// the retry rebuilds the usage, turn count and seen maps, so its result equals a clean scan.
func TestScanTranscriptIndexRetryAfterChangeEqualsACleanScan(t *testing.T) {
	shared, config := scanRetryFixture(t)
	clean, err := scanTranscriptIndex(shared, config)
	if err != nil {
		t.Fatal(err)
	}
	if clean.usage.CompletedResponses == 0 || clean.usage.TurnCount == 0 {
		t.Fatalf("clean scan has no usage to compare: %+v", clean.usage)
	}
	if err := os.Remove(config.UsageCachePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	calls := 0
	var firstVisited int
	installScanSnapshotRead(t, func(store *transcript.TurnStore, start, end int64, visit func(int64, []byte) error) error {
		calls++
		if calls > 1 {
			return (*transcript.TurnStore).ReadRecordsSnapshot(store, start, end, visit)
		}
		if err := (*transcript.TurnStore).ReadRecordsSnapshot(store, start, end, func(offset int64, line []byte) error {
			firstVisited++
			return visit(offset, line)
		}); err != nil {
			return err
		}
		return transcript.ErrTranscriptChanged
	})

	retried, err := scanTranscriptIndex(shared, config)
	if err != nil {
		t.Fatalf("scanTranscriptIndex() after one change: %v", err)
	}
	if calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2 (one discarded attempt, one retry)", calls)
	}
	if firstVisited == 0 {
		t.Fatal("the discarded attempt visited no records, so it did not exercise the reset")
	}
	if !reflect.DeepEqual(retried.usage, clean.usage) || retried.turns != clean.turns || retried.lastResponseKey != clean.lastResponseKey || retried.size != clean.size {
		t.Fatalf("retried scan = %+v (turns %d, last %q), want clean scan %+v (turns %d, last %q)",
			retried.usage, retried.turns, retried.lastResponseKey, clean.usage, clean.turns, clean.lastResponseKey)
	}
}

// Two changes in a row must surface as an error that wraps ErrTranscriptChanged, after exactly
// two reads, and must not write a usage cache.
func TestScanTranscriptIndexReportsASecondChangeAndWritesNoCache(t *testing.T) {
	shared, config := scanRetryFixture(t)
	calls := 0
	installScanSnapshotRead(t, func(*transcript.TurnStore, int64, int64, func(int64, []byte) error) error {
		calls++
		return transcript.ErrTranscriptChanged
	})

	_, err := scanTranscriptIndex(shared, config)
	if !errors.Is(err, transcript.ErrTranscriptChanged) {
		t.Fatalf("scanTranscriptIndex() error = %v, want one that wraps ErrTranscriptChanged", err)
	}
	if calls != 2 {
		t.Fatalf("snapshot reads = %d, want exactly 2", calls)
	}
	if usageCacheExists(t, config) {
		t.Fatal("a failed scan wrote a usage cache")
	}
}
