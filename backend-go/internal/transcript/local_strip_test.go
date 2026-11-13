package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMeta = `,"` + MetaField + `":{"` + MetaStoredAt + `":"2026-11-13T07:01:02.345Z"}`

// withTestMeta appends testMeta to every user message record of the transcript at path, as the
// writer does, and returns the file before the change.
func withTestMeta(t *testing.T, path string) []byte {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, line := range strings.SplitAfter(string(before), "\n") {
		if strings.Contains(line, `"role":"user"`) {
			line = strings.TrimSuffix(line, "}\n") + testMeta + "}\n"
		}
		out = append(out, line...)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return before
}

// strip-local blanks the steer marker in place: the file keeps its size and every other byte,
// each record stays valid JSON, the history read from it is the same, and no record is marked.
func TestStripLocalFieldsBlanksTheMarkerInPlace(t *testing.T) {
	f := newSteeredFixture(t, true)
	path := filepath.Join(f.dir, "messages.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	history := f.history(t)
	if counts, err := StripLocalFields(path, false, false); err != nil || counts != (LocalStripCounts{Records: 1, Steer: 1}) {
		t.Fatalf("dry run = %+v, %v; want 1 steer", counts, err)
	}
	if unchanged, _ := os.ReadFile(path); !bytes.Equal(unchanged, before) {
		t.Fatal("the dry run changed the file")
	}
	if counts, err := StripLocalFields(path, true, false); err != nil || counts != (LocalStripCounts{Records: 1, Steer: 1}) {
		t.Fatalf("strip = %+v, %v; want 1 steer", counts, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("size %d, want %d", len(after), len(before))
	}
	steerSuffix := `,"` + SessionInputSteerField + `":true}`
	beforeLines, afterLines := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	changed := 0
	for index, line := range afterLines {
		if line != beforeLines[index] {
			changed++
			if strings.TrimRight(line, " }") != strings.TrimSuffix(beforeLines[index], steerSuffix) {
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
	if counts, err := StripLocalFields(path, true, false); err != nil || counts != (LocalStripCounts{}) {
		t.Fatalf("second strip = %+v, %v; want 0", counts, err)
	}
}

// With MetaField after the steer marker, strip-local blanks both and --meta-only only the
// meta: the file keeps its size and offsets, the result reads like the transcript that never
// had the field (the same compact bytes), the steer marker still continues the turn, and a
// second run finds nothing.
func TestStripLocalFieldsBlanksMetaWithAndWithoutTheSteerMarker(t *testing.T) {
	for _, metaOnly := range []bool{true, false} {
		f := newSteeredFixture(t, true)
		path := filepath.Join(f.dir, "messages.jsonl")
		history := f.history(t)
		withoutMeta := withTestMeta(t, path)
		withMeta, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.history(t); got != history {
			t.Fatalf("meta changed the history:\n%s\nwant\n%s", got, history)
		}
		want := LocalStripCounts{Records: 3, Meta: 3}
		if !metaOnly {
			want.Steer = 1
		}
		if counts, err := StripLocalFields(path, true, metaOnly); err != nil || counts != want {
			t.Fatalf("metaOnly=%v: strip = %+v, %v; want %+v", metaOnly, counts, err, want)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(withMeta) || strings.Contains(string(after), MetaField) {
			t.Fatalf("metaOnly=%v: size %d want %d, file:\n%s", metaOnly, len(after), len(withMeta), after)
		}
		withMetaLines, afterLines, plainLines := strings.Split(string(withMeta), "\n"), strings.Split(string(after), "\n"), strings.Split(string(withoutMeta), "\n")
		for index, line := range afterLines {
			if len(line) != len(withMetaLines[index]) {
				t.Fatalf("record %d moved", index)
			}
			if line == "" {
				continue
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(line)); err != nil {
				t.Fatal(err)
			}
			want := plainLines[index]
			if !metaOnly {
				want = strings.Replace(want, `,"`+SessionInputSteerField+`":true`, "", 1)
			}
			if compact.String() != want {
				t.Fatalf("metaOnly=%v: record %d reads %s, want %s", metaOnly, index, compact.String(), want)
			}
		}
		if got := f.history(t); got != history {
			t.Fatalf("history changed:\n%s\nwant\n%s", got, history)
		}
		if strings.Contains(string(after), SessionInputSteerField) != metaOnly {
			t.Fatalf("metaOnly=%v: steer marker kept = %v", metaOnly, !metaOnly)
		}
		if counts, err := StripLocalFields(path, true, metaOnly); err != nil || counts.Records != 0 {
			t.Fatalf("second strip = %+v, %v; want 0", counts, err)
		}
	}
}

// A malformed meta value is still JAD's own trailing member, so it is blanked; a line that is
// not a JSON object (a torn tail) is left alone.
func TestStripLocalFieldsBlanksAMalformedMetaValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	data := `{"type":"message","role":"user","content":"x","` + MetaField + `":"not an object"}` + "\n" +
		`{"type":"message","role":"user","content":"y","` + MetaField + `":[1,{"a":null}]}` + "\n" +
		`{"type":"message","role":"user","content":"z","` + MetaField + `":{"stored_at":`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if counts, err := StripLocalFields(path, true, true); err != nil || counts != (LocalStripCounts{Records: 2, Meta: 2}) {
		t.Fatalf("strip = %+v, %v", counts, err)
	}
	after, _ := os.ReadFile(path)
	lines := strings.Split(string(after), "\n")
	for index, want := range []string{`{"type":"message","role":"user","content":"x"}`, `{"type":"message","role":"user","content":"y"}`} {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(lines[index])); err != nil || compact.String() != want {
			t.Fatalf("record %d = %q (%v)", index, lines[index], err)
		}
	}
	if !strings.HasSuffix(string(after), `"`+MetaField+`":{"stored_at":`) || len(after) != len(data) {
		t.Fatalf("the torn tail changed: %q", after)
	}
}

func TestStripLocalFieldsRefusesAFieldItDoesNotRecognize(t *testing.T) {
	steer := `"` + SessionInputSteerField + `"`
	meta := `"` + MetaField + `"`
	for name, record := range map[string]string{
		"steer not last":        `{"type":"message",` + steer + `:true,"role":"user","content":"x"}`,
		"steer false":           `{"type":"message","role":"user","content":"x",` + steer + `:false}`,
		"meta not last":         `{"type":"message",` + meta + `:{},"role":"user","content":"x"}`,
		"meta before steer":     `{"type":"message","role":"user","content":"x",` + meta + `:{},` + steer + `:true}`,
		"meta twice":            `{"type":"message","role":"user",` + meta + `:{},"content":"x",` + meta + `:{}}`,
		"escaped meta not last": `{"type":"message","\u005f_agent_meta":{},"role":"user","content":"x"}`,
	} {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		marked := `{"type":"message","role":"user","content":"y",` + steer + `:true` + testMeta + `}`
		data := []byte(marked + "\n" + record + "\n")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, metaOnly := range []bool{true, false} {
			if _, err := StripLocalFields(path, true, metaOnly); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("%s: err = %v", name, err)
			}
			if unchanged, _ := os.ReadFile(path); !bytes.Equal(unchanged, data) {
				t.Fatalf("%s: the file changed", name)
			}
		}
	}
}
