package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCodeContextRelocationNeedsASingleExactCopy(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("c.go", "x1\nx2\nx3\nx4\n")
	f.call(`{"action":"read","task":"T-10","path":"c.go","start":2,"end":3,"priority":1,"description":"the pair"}`)

	// The saved bytes now occur twice: the tool cannot tell which is meant.
	f.write("c.go", "top\nnext\nx2\nx3\nmid\nx2\nx3\n")
	if got := f.call(`{"action":"get","task":"T-10"}`); !strings.Contains(got, "1 stale entries") || strings.Contains(got, "relocated") {
		t.Fatalf("two copies were taken as a move:\n%s", got)
	}
	// Same line count and length but other bytes is not the code either.
	f.write("c.go", "a\nb\nx2\nx9\n")
	if got := f.call(`{"action":"get","task":"T-10"}`); !strings.Contains(got, "1 stale entries") {
		t.Fatalf("other bytes of the same length were taken as a move:\n%s", got)
	}
	// One copy, at the end of the file: a move.
	f.write("c.go", "a\nb\nc\nx2\nx3\n")
	if got := f.call(`{"action":"get","task":"T-10"}`); codeBlock(t, got, "c.go") != "x2\nx3\n" || !strings.Contains(got, "lines 4-5") {
		t.Fatalf("a single copy was not relocated:\n%s", got)
	}

	// With relocate off a move is stale.
	off := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.Relocate = false })
	off.write("c.go", "x1\nx2\nx3\n")
	off.call(`{"action":"read","task":"T-10","path":"c.go","start":2,"end":3,"priority":1,"description":"the pair"}`)
	off.write("c.go", "new\nx1\nx2\nx3\n")
	if got := off.call(`{"action":"get","task":"T-10"}`); !strings.Contains(got, "1 stale entries") {
		t.Fatalf("relocate false still moved the entry:\n%s", got)
	}
}

func TestCodeContextRelocationOntoAnotherEntryIsStale(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("d.go", "k1\nk2\nk3\n")
	f.call(`{"action":"read","task":"T-11","path":"d.go","start":2,"end":3,"priority":1,"description":"old"}`)
	f.write("d.go", "new\nk1\nk2\nk3\n")
	// Before any check, a second entry is saved where the code now is.
	f.call(`{"action":"read","task":"T-11","path":"d.go","start":3,"end":4,"priority":1,"description":"new"}`)
	got := f.call(`{"action":"get","task":"T-11"}`)
	if !strings.Contains(got, "#1 p1 d.go:2-3 [stale: the code moved to lines 3-4, which entry #2 reads; drop this entry]  old\n") || strings.Count(got, "--- code ") != 1 {
		t.Fatalf("a relocation onto entry #2 was not refused:\n%s", got)
	}
}

func TestRelocateCodeContextLineOffsets(t *testing.T) {
	for _, tc := range []struct {
		data string
		want []int
	}{
		{"", []int{0}},
		{"a", []int{0, 1}},
		{"a\n", []int{0, 2}},
		{"a\nb", []int{0, 2, 3}},
		{"a\n\nb\n", []int{0, 2, 3, 5}},
	} {
		file := &codeContextFile{data: []byte(tc.data)}
		got := file.lineOffsets()
		if len(got) != len(tc.want) {
			t.Errorf("lineOffsets(%q) = %v, want %v", tc.data, got, tc.want)
			continue
		}
		for index := range got {
			if got[index] != tc.want[index] {
				t.Errorf("lineOffsets(%q) = %v, want %v", tc.data, got, tc.want)
				break
			}
		}
	}
	content := []byte("x2\nx3")
	entry := codeContextEntry{Start: 1, End: 2, ContentSHA256: codeContextSHA256(content), ContentBytes: int64(len(content)), HeadSHA256: codeContextSHA256([]byte("x2\n"))}
	start, moved, found := relocateCodeContext(&codeContextFile{data: []byte("a\nx2\nx3")}, entry)
	if !found || start != 2 || string(moved) != "x2\nx3" {
		t.Fatalf("relocate to the last line without a newline = %d %q %v", start, moved, found)
	}
	// An entry written before content_bytes was kept is never relocated.
	entry.ContentBytes = 0
	if _, _, found := relocateCodeContext(&codeContextFile{data: []byte("a\nx2\nx3")}, entry); found {
		t.Fatal("an entry without content_bytes was relocated")
	}
}

func TestCodeContextReadPrintsAndSaves(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxEntries = 2 })
	f.write("r.go", numbered("r", 20))

	read := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`)
	if code := codeBlock(t, read, "r.go"); code != "r3\nr4\nr5\nr6\n" ||
		!strings.HasPrefix(read, "code_context task T-15, generation 1: #1 saved, no description yet\n=== r.go lines 3-6 ===\n#1 p6 lines 3-6  (no description)\n--- code ") {
		t.Fatalf("read:\n%s", read)
	}
	if listed := f.call(`{"action":"list","task":"T-15"}`); !strings.Contains(listed, "\n#1 p6 r.go:3-6  (no description)\n") {
		t.Fatalf("list of an undescribed entry:\n%s", listed)
	}

	// The same range again, and lines inside it: printed, not saved again.
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`); !strings.Contains(got, "generation 1: already saved as #1") || codeBlock(t, got, "r.go") != "r3\nr4\nr5\nr6\n" {
		t.Fatalf("read of a saved range:\n%s", got)
	}
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":4,"end":5}`); !strings.Contains(got, "these lines are inside #1 (lines 3-6)") || codeBlock(t, got, "r.go") != "r4\nr5\n" {
		t.Fatalf("read inside a saved range:\n%s", got)
	}
	// A description on the same range describes the entry, and the code is not printed:
	// the caller has seen it.
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6,"description":"the loop\nand its exit","priority":2}`); got != "code_context task T-15, generation 2: #1 updated\n#1 p2 r.go:3-6  the loop\n  and its exit\n" {
		t.Fatalf("read with a description:\n%s", got)
	}

	// Reading the exact range of a stale entry refreshes it.
	f.write("r.go", strings.Replace(numbered("r", 20), "r4\n", "r4 edited\n", 1))
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`); !strings.Contains(got, "#1 held other code; it now holds the code below") || codeBlock(t, got, "r.go") != "r3\nr4 edited\nr5\nr6\n" {
		t.Fatalf("read of a stale entry's range:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-15"}`); strings.Contains(got, "stale") {
		t.Fatalf("the refreshed entry is still stale:\n%s", got)
	}

	// Beyond max_entries the code is still printed; with a description nothing is
	// printed, so not saving it is an error.
	f.call(`{"action":"read","task":"T-15","path":"r.go","start":10,"end":11}`)
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":15,"end":16}`); !strings.Contains(got, "not saved: the task already has 2 entries; max_entries is 2") || codeBlock(t, got, "r.go") != "r15\nr16\n" || strings.Contains(got, "#3") {
		t.Fatalf("read beyond max_entries:\n%s", got)
	}
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":15,"end":16,"description":"d"}`); !strings.Contains(got, "error_code: too_many_entries") {
		t.Fatalf("a described read beyond max_entries: %s", got)
	}
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":30,"end":31}`); !strings.Contains(got, "error_code: range_outside_file") {
		t.Fatalf("read outside the file: %s", got)
	}
}

func TestCodeContextFindSearchesTheTask(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxFindResults = 2 })
	f.write("glob_match.go", numbered("g", 10))
	f.write("other.go", numbered("o", 10))
	f.call(`{"action":"read","task":"T-20","priority":3,"items":[
		{"path":"glob_match.go","start":2,"end":4,"priority":1,"description":"expandGlobBraces caps the brace expansion at 64 patterns."},
		{"path":"other.go","start":1,"end":2,"description":"Glob options reach the walker here."},
		{"path":"other.go","start":5,"end":6,"priority":2,"description":"Glob walker, soon edited."}]}`)

	if got := f.call(`{"action":"find","task":"T-20","query":"glob cap."}`); got != "code_context find \"glob cap.\" in task T-20: 1 entries match every word\n#1 p1 glob_match.go:2-4  expandGlobBraces caps the brace expansion at 64 patterns.\n" {
		t.Fatalf("find every word:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"options brace"}`); !strings.Contains(got, "no entry matches every word; 2 match some") {
		t.Fatalf("find some words:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"glob_match.go"}`); !strings.Contains(got, "\n#1 p1 glob_match.go:2-4  ") {
		t.Fatalf("find by file name:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"nothing"}`); !strings.Contains(got, ": no entry matches") {
		t.Fatalf("find without a match:\n%s", got)
	}
	// A match is checked against its file; max_find_results bounds what is shown.
	f.write("other.go", strings.Replace(numbered("o", 10), "o5\n", "o5 edited\n", 1))
	got := f.call(`{"action":"find","task":"T-20","query":"glob"}`)
	if !strings.Contains(got, ": 2 entries match every word\n1 more matches left out by max_find_results; narrow the query\n") ||
		!strings.Contains(got, "#3 p2 other.go:5-6 [stale: the file changed and these lines differ from what was saved]  Glob walker, soon edited.\n") {
		t.Fatalf("find of a stale entry, past max_find_results:\n%s", got)
	}
}

// The fields and actions the tool dropped are refused, so a caller learns they are gone.
func TestCodeContextRemovedFieldsAreRefused(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("a.go", numbered("a", 3))
	f.call(`{"action":"read","task":"T-40","path":"a.go","start":1,"end":2,"description":"d"}`)
	for _, arguments := range []string{
		`{"action":"add","task":"T-40","path":"a.go","start":1,"end":2,"priority":1,"description":"d"}`,
		`{"action":"get","task":"T-40","since":"000000000000"}`,
		`{"action":"get","task":"T-40","mode":"outline"}`,
		`{"action":"get","task":"T-40","max_priority":2}`,
		`{"action":"find","task":"T-40","query":"d","all_tasks":true}`,
		`{"action":"read","task":"T-41","from_task":"T-40","id":1}`,
		`{"action":"read","task":"T-41","items":[{"from_task":"T-40","id":1}]}`,
	} {
		if got := f.call(arguments); !strings.Contains(got, "validation_failed") {
			t.Fatalf("%s was not refused:\n%s", arguments, got)
		}
	}
}
