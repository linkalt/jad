package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func snapshotTestJournal(t *testing.T, content string) *Journal {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func collectRecords(t *testing.T, read func(visit func(offset int64, line []byte) error) error) ([]int64, []string) {
	t.Helper()
	var offsets []int64
	var lines []string
	if err := read(func(offset int64, line []byte) error {
		offsets = append(offsets, offset)
		lines = append(lines, string(line))
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	return offsets, lines
}

// An append that runs while the snapshot is being visited writes past the snapshot end, so
// the records visited match a read taken before the append, with none double-counted.
func TestReadRecordsSnapshotIgnoresAppendsDuringTheRead(t *testing.T) {
	base := "{\"n\":1}\n{\"n\":2}\n"
	journal := snapshotTestJournal(t, base)
	_, want := collectRecords(t, func(visit func(int64, []byte) error) error {
		return journal.ReadRecordsSnapshot(0, 1<<63-1, visit)
	})

	journal = snapshotTestJournal(t, base)
	appended := false
	offsets, got := collectRecords(t, func(visit func(int64, []byte) error) error {
		return journal.ReadRecordsSnapshot(0, 1<<63-1, func(offset int64, line []byte) error {
			if !appended {
				appended = true
				if err := journal.Append([]byte(`{"n":3}`), []byte(`{"n":4}`)); err != nil {
					t.Fatal(err)
				}
			}
			return visit(offset, line)
		})
	})

	if len(got) != 2 || len(want) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("records during append = %q, want %q", got, want)
	}
	if want := []int64{0, 8}; len(offsets) != 2 || offsets[0] != want[0] || offsets[1] != want[1] {
		t.Fatalf("offsets = %v, want %v", offsets, want)
	}
	if data := readTranscript(t, journal); data != base+"{\"n\":3}\n{\"n\":4}\n" {
		t.Fatalf("transcript after append = %q", data)
	}
}

// A snapshot that ends inside a record stops at the last newline, so a torn final line is not
// visited. A ranged read uses the same snapshot end, so the torn bytes are not visited there either.
func TestReadRecordsSnapshotStopsAtATornFinalLine(t *testing.T) {
	journal := snapshotTestJournal(t, "{\"n\":1}\n{\"n\":2")
	offsets, lines := collectRecords(t, func(visit func(int64, []byte) error) error {
		return journal.ReadRecordsSnapshot(0, 1<<63-1, visit)
	})
	if len(lines) != 1 || lines[0] != `{"n":1}` || offsets[0] != 0 {
		t.Fatalf("records = %v at %v, want the first complete record only", lines, offsets)
	}
	if data := readTranscript(t, journal); data != "{\"n\":1}\n" {
		t.Fatalf("torn tail was not repaired before the snapshot: %q", data)
	}
}

// A snapshot read that sees the transcript replaced by a rename must fail with
// ErrTranscriptChanged, so the caller can discard the records and retry.
func TestReadRecordsSnapshotReportsReplacementForRetry(t *testing.T) {
	journal := snapshotTestJournal(t, "{\"n\":1}\n{\"n\":2}\n")
	replaced := false
	visited := 0
	err := journal.ReadRecordsSnapshot(0, 1<<63-1, func(int64, []byte) error {
		visited++
		if !replaced {
			replaced = true
			replacement := filepath.Join(filepath.Dir(journal.path), "replacement.jsonl")
			if err := os.WriteFile(replacement, []byte("{\"n\":9}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, journal.path); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if !errors.Is(err, ErrTranscriptChanged) {
		t.Fatalf("ReadRecordsSnapshot() error = %v, want ErrTranscriptChanged", err)
	}
	if visited == 0 {
		t.Fatal("the visitor should have run before the change was detected")
	}

	// The retry reads the replacement from the start.
	_, lines := collectRecords(t, func(visit func(int64, []byte) error) error {
		return journal.ReadRecordsSnapshot(0, 1<<63-1, visit)
	})
	if len(lines) != 1 || lines[0] != `{"n":9}` {
		t.Fatalf("retry records = %v, want the replacement", lines)
	}
}

// A compaction bumps the journal generation. A snapshot read that sees it must fail even when
// the file identity still matches.
func TestReadRecordsSnapshotReportsGenerationChangeForRetry(t *testing.T) {
	journal := snapshotTestJournal(t, "{\"n\":1}\n{\"n\":2}\n")
	changed := false
	err := journal.ReadRecordsSnapshot(0, 1<<63-1, func(int64, []byte) error {
		if !changed {
			changed = true
			journal.generation++
		}
		return nil
	})
	if !errors.Is(err, ErrTranscriptChanged) {
		t.Fatalf("ReadRecordsSnapshot() error = %v, want ErrTranscriptChanged", err)
	}
}

// Bytes written after the snapshot is taken, with no newline yet, are past the snapshot end and
// are not visited, even though they are visible in the file.
func TestReadRecordsSnapshotDoesNotVisitAPartialAppendAfterTheSnapshot(t *testing.T) {
	journal := snapshotTestJournal(t, "{\"n\":1}\n")
	wrote := false
	_, lines := collectRecords(t, func(visit func(int64, []byte) error) error {
		return journal.ReadRecordsSnapshot(0, 1<<63-1, func(offset int64, line []byte) error {
			if !wrote {
				wrote = true
				file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString(`{"n":2`); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			return visit(offset, line)
		})
	})
	if len(lines) != 1 || lines[0] != `{"n":1}` {
		t.Fatalf("records = %q, want only the complete record", lines)
	}
}
