package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNoEmptyLine fails unless every line of the transcript is one JSON object and the file ends in
// exactly one newline. Empty lines are what the usage scan and FrontCompactFromMessageWith cannot
// account for, so this is the property that keeps them out of reach of the backend's own writes.
func assertNoEmptyLine(t *testing.T, label, data string) {
	t.Helper()
	if data == "" {
		return
	}
	if !strings.HasSuffix(data, "\n") || strings.HasSuffix(data, "\n\n") {
		t.Fatalf("%s: transcript %q does not end in exactly one newline", label, data)
	}
	for number, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		var object map[string]any
		if line == "" || json.Unmarshal([]byte(line), &object) != nil {
			t.Fatalf("%s: line %d = %q, want one JSON object", label, number+1, line)
		}
	}
}

// Whatever spacing a caller puts around or inside a record, a write stores it as one compact line;
// a record that is empty, blank or not an object is refused before the file is touched.
func TestJournalWritesNeverProduceAnEmptyLine(t *testing.T) {
	spaced := []string{"  {\"a\":1}\n\n", "\r\n{\n\"b\" :\n2}\r\n", "\t{ \"c\" : [ 1 ,\n 2 ] }\n"}
	refused := [][]byte{nil, []byte(""), []byte("\n"), []byte(" \r\n\t "), []byte("[]"), []byte("{\"a\":\"x\ny\"}"), []byte("{\"a\":1}\n{\"b\":2}")}

	for name, write := range map[string]func(*Journal, ...[]byte) error{
		"append":  (*Journal).Append,
		"replace": (*Journal).ReplaceRecords,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "messages.jsonl")
			journal, err := NewJournal(path, DurabilityProcess)
			if err != nil {
				t.Fatal(err)
			}
			records := make([][]byte, len(spaced))
			for index, text := range spaced {
				records[index] = []byte(text)
			}
			if err := write(journal, records...); err != nil {
				t.Fatal(err)
			}
			stored := readTranscript(t, journal)
			assertNoEmptyLine(t, name, stored)
			if want := "{\"a\":1}\n{\"b\":2}\n{\"c\":[1,2]}\n"; stored != want {
				t.Fatalf("stored = %q, want %q", stored, want)
			}
			for _, record := range refused {
				// Alone and after a good record: the batch lands together or not at all.
				for _, batch := range [][][]byte{{record}, {[]byte("{\"d\":4}"), record}} {
					if err := write(journal, batch...); err == nil {
						t.Fatalf("%s %q: no error", name, record)
					}
					if got := readTranscript(t, journal); got != stored {
						t.Fatalf("%s %q changed the transcript: %q", name, record, got)
					}
				}
			}
			// Nothing is left beside the transcript.
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("directory = %v, want only the transcript", entries)
			}
		})
	}
}

// A crash can leave a prefix of a batch. Cut at every byte, the repair and the next write leave a
// transcript with no empty line, whether the cut falls inside a record or right after a newline.
func TestEveryTornPrefixOfABatchRepairsToLinesWithoutAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	first, second := `{"type":"a","n":1}`, `{"type":"b","n":2}`
	if err := journal.Append([]byte(first), []byte(second)); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut <= len(full); cut++ {
		// Also the shape a lost write can leave: the cut prefix and a hole of zero bytes.
		for _, hole := range [][]byte{nil, make([]byte, 5)} {
			torn := append(append([]byte{}, full[:cut]...), hole...)
			if err := os.WriteFile(path, torn, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.RepairTail(); err != nil {
				t.Fatalf("cut %d: %v", cut, err)
			}
			assertNoEmptyLine(t, "after repair", readTranscript(t, journal))
			if err := journal.Append([]byte(`{"type":"c","n":3}`)); err != nil {
				t.Fatalf("cut %d: append: %v", cut, err)
			}
			got := readTranscript(t, journal)
			assertNoEmptyLine(t, "after append", got)
			if !bytes.HasSuffix([]byte(got), []byte("{\"type\":\"c\",\"n\":3}\n")) {
				t.Fatalf("cut %d: transcript = %q", cut, got)
			}
		}
	}
}
