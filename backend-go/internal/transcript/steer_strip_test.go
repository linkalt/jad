package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// strip-steer blanks the marker in place: the file keeps its size and every other byte, each
// record stays valid JSON, the history read from it is the same, and no record is marked.
func TestStripSteerMarksBlanksTheMarkerInPlace(t *testing.T) {
	f := newSteeredFixture(t, true)
	path := filepath.Join(f.dir, "messages.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	history := f.history(t)
	if count, err := StripSteerMarks(path, false); err != nil || count != 1 {
		t.Fatalf("dry run = %d, %v; want 1", count, err)
	}
	if unchanged, _ := os.ReadFile(path); !bytes.Equal(unchanged, before) {
		t.Fatal("the dry run changed the file")
	}
	if count, err := StripSteerMarks(path, true); err != nil || count != 1 {
		t.Fatalf("strip = %d, %v; want 1", count, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("size %d, want %d", len(after), len(before))
	}
	beforeLines, afterLines := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	changed := 0
	for index, line := range afterLines {
		if line != beforeLines[index] {
			changed++
			if strings.TrimRight(line, " }") != strings.TrimSuffix(beforeLines[index], string(steerMarkSuffix)) {
				t.Fatalf("record %d became %q from %q", index, line, beforeLines[index])
			}
		}
		if line != "" && (!json.Valid([]byte(line)) || recordIsSteered([]byte(line))) {
			t.Fatalf("record %d is %q", index, line)
		}
	}
	if changed != 1 || strings.Contains(string(after), SessionInputSteerField) {
		t.Fatalf("changed %d records; file:\n%s", changed, after)
	}
	if got := f.history(t); got != history {
		t.Fatalf("history changed:\n%s\nwant\n%s", got, history)
	}
	if count, err := StripSteerMarks(path, true); err != nil || count != 0 {
		t.Fatalf("second strip = %d, %v; want 0", count, err)
	}
}

// A marker anywhere but last, or not true, is refused and nothing is written.
func TestStripSteerMarksRefusesAMarkerItDoesNotRecognize(t *testing.T) {
	for name, record := range map[string]string{
		"not last": `{"type":"message","` + SessionInputSteerField + `":true,"role":"user","content":"x"}`,
		"false":    `{"type":"message","role":"user","content":"x","` + SessionInputSteerField + `":false}`,
	} {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		marked := `{"type":"message","role":"user","content":"y","` + SessionInputSteerField + `":true}`
		data := []byte(marked + "\n" + record + "\n")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := StripSteerMarks(path, true); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("%s: err = %v", name, err)
		}
		if unchanged, _ := os.ReadFile(path); !bytes.Equal(unchanged, data) {
			t.Fatalf("%s: the file changed", name)
		}
	}
}
