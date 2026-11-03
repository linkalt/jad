package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderedLine(t *testing.T, dir, name, line string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendRendered(t *testing.T, journal *Journal, path string) bool {
	t.Helper()
	inserted, err := journal.appendFileIfAbsent(path)
	if err != nil {
		t.Fatalf("appendFileIfAbsent(%s): %v", path, err)
	}
	return inserted
}

func readTranscript(t *testing.T, journal *Journal) string {
	t.Helper()
	data, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newAppendTestJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	return journal, dir
}

const lineA = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A"}]}`
const lineB = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"B"}]}`

func TestAppendFileIfAbsentStaysIdempotent(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	b := renderedLine(t, dir, "b.jsonl", lineB)

	if !appendRendered(t, journal, a) || !appendRendered(t, journal, b) {
		t.Fatal("first appends must insert")
	}
	if appendRendered(t, journal, a) || appendRendered(t, journal, b) {
		t.Fatal("repeated appends must report already present")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"+lineB+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileIfAbsentSeesRecordsAppendedByJournalAppend(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	if !appendRendered(t, journal, renderedLine(t, dir, "a.jsonl", lineA)) {
		t.Fatal("first append must insert")
	}
	if err := journal.Append([]byte(lineB)); err != nil {
		t.Fatal(err)
	}
	if appendRendered(t, journal, renderedLine(t, dir, "b.jsonl", lineB)) {
		t.Fatal("a line written by Append must be found")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"+lineB+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileIfAbsentSeesAFileRewrittenThroughJournal(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	if !appendRendered(t, journal, a) {
		t.Fatal("first append must insert")
	}
	if err := journal.ReplaceRecords(); err != nil {
		t.Fatal(err)
	}
	if !appendRendered(t, journal, a) {
		t.Fatal("a line removed by ReplaceRecords must be inserted again")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileIfAbsentSeesFrontCompaction(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	b := renderedLine(t, dir, "b.jsonl", lineB)
	if !appendRendered(t, journal, a) || !appendRendered(t, journal, b) {
		t.Fatal("first appends must insert")
	}
	size := int64(len(lineA) + len(lineB) + 2)
	if err := journal.ReplaceWithPrefix(int64(len(lineA)+1), size, ""); err != nil {
		t.Fatal(err)
	}
	if appendRendered(t, journal, a) {
		t.Fatal("a line kept by the prefix must still be present")
	}
	if !appendRendered(t, journal, b) {
		t.Fatal("a line removed by front compaction must be inserted again")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"+lineB+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileIfAbsentSeesAFileReplacedOrShrunkOutsideJournal(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, journal *Journal, dir string)
	}{
		{"renamed over", func(t *testing.T, journal *Journal, dir string) {
			replacement := filepath.Join(dir, "other.jsonl")
			if err := os.WriteFile(replacement, []byte(lineB+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, journal.path); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", func(t *testing.T, journal *Journal, dir string) {
			if err := os.Truncate(journal.path, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"tail edited in place", func(t *testing.T, journal *Journal, dir string) {
			data := []byte(strings.Replace(readTranscript(t, journal), `"A"`, `"Z"`, 1))
			if err := os.WriteFile(journal.path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, dir := newAppendTestJournal(t)
			a := renderedLine(t, dir, "a.jsonl", lineA)
			if !appendRendered(t, journal, a) {
				t.Fatal("first append must insert")
			}
			test.mutate(t, journal, dir)
			if !appendRendered(t, journal, a) {
				t.Fatal("a line that is no longer in the file must be inserted")
			}
			if !strings.Contains(readTranscript(t, journal), lineA+"\n") {
				t.Fatalf("transcript lacks the re-inserted line: %q", readTranscript(t, journal))
			}
		})
	}
}

func TestAppendFileIfAbsentRepairsPartialTailBeforeChecking(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	b := renderedLine(t, dir, "b.jsonl", lineB)
	if !appendRendered(t, journal, a) {
		t.Fatal("first append must insert")
	}
	file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"partial`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if !appendRendered(t, journal, b) {
		t.Fatal("append after a partial tail must insert")
	}
	if appendRendered(t, journal, a) || appendRendered(t, journal, b) {
		t.Fatal("both lines must be found afterwards")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"+lineB+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileIfAbsentFindsLinesLongerThanReaderBuffer(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	long := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("x", 200_000) + `"}]}`
	path := renderedLine(t, dir, "long.jsonl", long)
	if !appendRendered(t, journal, path) {
		t.Fatal("first append must insert")
	}
	if appendRendered(t, journal, path) {
		t.Fatal("a long line must be found by its hash")
	}
}

func appendUnlessLast(t *testing.T, journal *Journal, path string) bool {
	t.Helper()
	inserted, err := journal.appendFileUnlessLast(path)
	if err != nil {
		t.Fatalf("appendFileUnlessLast(%s): %v", path, err)
	}
	return inserted
}

// TestAppendFileUnlessLastSkipsOnlyAnIdenticalLastLine pins the live-commit check: it
// compares the line with the end of the transcript only, as one whole line.
func TestAppendFileUnlessLastSkipsOnlyAnIdenticalLastLine(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	b := renderedLine(t, dir, "b.jsonl", lineB)
	if !appendUnlessLast(t, journal, a) {
		t.Fatal("the first line of an empty transcript must be appended")
	}
	if appendUnlessLast(t, journal, a) {
		t.Fatal("a line identical to the last line must not be appended again")
	}
	if !appendUnlessLast(t, journal, b) || !appendUnlessLast(t, journal, a) {
		t.Fatal("a line that is not the last line must be appended")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"+lineB+"\n"+lineA+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestAppendFileUnlessLastComparesWholeLines(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	tail := `{"text":"B"}`
	if err := journal.Append([]byte(`{"x":1,"text":"B"}`)); err != nil {
		t.Fatal(err)
	}
	// The last line ends with the same bytes, but it is a longer line.
	if err := os.WriteFile(filepath.Join(dir, "suffix.jsonl"), []byte(`1,"text":"B"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !appendUnlessLast(t, journal, filepath.Join(dir, "suffix.jsonl")) {
		t.Fatal("the end of a longer last line is not the same line")
	}
	long := `{"type":"message","content":"` + strings.Repeat("y", 300_000) + `"}`
	path := renderedLine(t, dir, "long.jsonl", long)
	if !appendUnlessLast(t, journal, path) || appendUnlessLast(t, journal, path) {
		t.Fatal("a line longer than the compare buffer must be appended once")
	}
	other := renderedLine(t, dir, "other.jsonl", strings.Replace(long, "yyy\"", "yyz\"", 1))
	if !appendUnlessLast(t, journal, other) {
		t.Fatal("a long line that differs in its last chunk must be appended")
	}
	if !appendUnlessLast(t, journal, renderedLine(t, dir, "tail.jsonl", tail)) {
		t.Fatal("a short line after a long one must be appended")
	}
}

func TestAppendFileUnlessLastRepairsPartialTailBeforeComparing(t *testing.T) {
	journal, dir := newAppendTestJournal(t)
	a := renderedLine(t, dir, "a.jsonl", lineA)
	if !appendUnlessLast(t, journal, a) {
		t.Fatal("first append must insert")
	}
	file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"partial`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if appendUnlessLast(t, journal, a) {
		t.Fatal("after the partial tail is cut, the last line is the same line")
	}
	if got, want := readTranscript(t, journal), lineA+"\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}
