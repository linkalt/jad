package native

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

var codeContextSincePattern = regexp.MustCompile(`(?m)^since: ([0-9a-f]{12}) `)

func sinceToken(t *testing.T, output string) string {
	t.Helper()
	match := codeContextSincePattern.FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("no since token in:\n%s", output)
	}
	return match[1]
}

func TestCodeContextRelocationNeedsASingleExactCopy(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("c.go", "x1\nx2\nx3\nx4\n")
	f.call(`{"action":"add","task":"T-10","path":"c.go","start":2,"end":3,"priority":1,"description":"the pair"}`)

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
	off.call(`{"action":"add","task":"T-10","path":"c.go","start":2,"end":3,"priority":1,"description":"the pair"}`)
	off.write("c.go", "new\nx1\nx2\nx3\n")
	if got := off.call(`{"action":"get","task":"T-10"}`); !strings.Contains(got, "1 stale entries") {
		t.Fatalf("relocate false still moved the entry:\n%s", got)
	}
}

func TestCodeContextRelocationOntoAnotherEntryIsStale(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("d.go", "k1\nk2\nk3\n")
	f.call(`{"action":"add","task":"T-11","path":"d.go","start":2,"end":3,"priority":1,"description":"old"}`)
	f.write("d.go", "new\nk1\nk2\nk3\n")
	// Before any check, a second entry is saved where the code now is.
	f.call(`{"action":"add","task":"T-11","path":"d.go","start":3,"end":4,"priority":1,"description":"new"}`)
	got := f.call(`{"action":"get","task":"T-11"}`)
	if !strings.Contains(got, "- id 1  d.go  (was 2-3, priority 1): the code moved to lines 3-4, which entry #2 reads; drop this entry") || strings.Count(got, "--- code ") != 1 {
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

func TestCodeContextGetSinceSkipsWhatTheCallerHolds(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("a.go", numbered("a", 10))
	f.write("b.go", numbered("b", 10))
	f.call(`{"action":"add","task":"T-12","path":"a.go","start":1,"end":3,"priority":1,"description":"first"}`)
	f.call(`{"action":"add","task":"T-12","path":"b.go","start":1,"end":3,"priority":2,"description":"second"}`)
	first := f.call(`{"action":"get","task":"T-12"}`)
	token := sinceToken(t, first)
	if !strings.Contains(first, "code bytes: 18 printed\n") {
		t.Fatalf("first get:\n%s", first)
	}

	// Nothing changed: no code, both named.
	again := f.call(`{"action":"get","task":"T-12","since":"` + token + `"}`)
	if strings.Contains(again, "--- code ") || !strings.Contains(again, "0 entries shown, 2 unchanged since "+token) ||
		!strings.Contains(again, "code bytes: 0 printed, 18 not printed again") || !strings.Contains(again, "- #1  a.go 1-3  (priority 1)") || !strings.Contains(again, "- #2  b.go 1-3  (priority 2)") {
		t.Fatalf("get since an unchanged delivery:\n%s", again)
	}

	// A new description and moved code: the first is printed again, the second is not.
	f.call(`{"action":"update","task":"T-12","id":1,"description":"first, reworded"}`)
	f.write("b.go", "inserted\n"+numbered("b", 10))
	changed := f.call(`{"action":"get","task":"T-12","since":"` + sinceToken(t, again) + `"}`)
	if !strings.Contains(changed, "first, reworded") || strings.Contains(changed, "=== b.go") || !strings.Contains(changed, "- #2  b.go 2-4  (priority 2, moved from 1-3)") {
		t.Fatalf("get since after a change:\n%s", changed)
	}
	// A token is good for any later get, not only the next one.
	if got := f.call(`{"action":"get","task":"T-12","since":"` + token + `"}`); !strings.Contains(got, "=== a.go") || strings.Contains(got, "=== b.go") {
		t.Fatalf("get since the first token:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-12","since":"000000000000"}`); !strings.Contains(got, "since 000000000000 is unknown or expired; every entry is printed") || strings.Count(got, "--- code ") != 2 {
		t.Fatalf("unknown since:\n%s", got)
	}
}

func TestCodeContextSinceTokensExpireOldestFirst(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxDeliveries = 2 })
	f.write("a.go", numbered("a", 3))
	f.call(`{"action":"add","task":"T-13","path":"a.go","start":1,"end":2,"priority":1,"description":"d"}`)
	oldest := sinceToken(t, f.call(`{"action":"get","task":"T-13"}`))
	f.call(`{"action":"get","task":"T-13"}`)
	newest := sinceToken(t, f.call(`{"action":"get","task":"T-13"}`))
	if got := f.call(`{"action":"get","task":"T-13","since":"` + oldest + `"}`); !strings.Contains(got, "is unknown or expired") {
		t.Fatalf("the oldest token outlived max_deliveries:\n%s", got)
	}
	latest := f.call(`{"action":"get","task":"T-13","since":"` + newest + `"}`)
	if !strings.Contains(latest, "1 unchanged since") {
		t.Fatalf("the newest token:\n%s", latest)
	}
	// clear removes the state with the entries: the same entry saved again is printed.
	f.call(`{"action":"clear","task":"T-13"}`)
	f.call(`{"action":"add","task":"T-13","path":"a.go","start":1,"end":2,"priority":1,"description":"d"}`)
	if got := f.call(`{"action":"get","task":"T-13","since":"` + sinceToken(t, latest) + `"}`); !strings.Contains(got, "is unknown or expired") {
		t.Fatalf("a token survived clear:\n%s", got)
	}
}

func TestCodeContextGetOutlineAndMaxPriority(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("a.go", numbered("a", 10))
	f.call(`{"action":"add","task":"T-14","path":"a.go","start":1,"end":2,"priority":1,"description":"target"}`)
	f.call(`{"action":"add","task":"T-14","path":"a.go","start":6,"end":7,"priority":5,"description":"tests"}`)
	outline := f.call(`{"action":"get","task":"T-14","mode":"outline"}`)
	if strings.Contains(outline, "--- code ") || !strings.Contains(outline, "outline of 2 entries") ||
		!strings.Contains(outline, "#1  a.go 1-2  (priority 1, updated by executor)\n  target\n") || !strings.Contains(outline, "#2  a.go 6-7  (priority 5, updated by executor)\n  tests\n") {
		t.Fatalf("outline:\n%s", outline)
	}
	if got := f.call(`{"action":"get","task":"T-14","max_priority":2}`); !strings.Contains(got, "=== a.go lines 1-2 ===") || strings.Contains(got, "lines 6-7") {
		t.Fatalf("get max_priority:\n%s", got)
	}
	if got := f.call(`{"action":"list","task":"T-14","max_priority":4}`); strings.Count(got, "  - id: ") != 1 {
		t.Fatalf("list max_priority:\n%s", got)
	}
}

func TestCodeContextReadPrintsAndSaves(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxEntries = 2 })
	f.write("r.go", numbered("r", 20))

	read := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`)
	if code := codeBlock(t, read, "r.go"); code != "r3\nr4\nr5\nr6\n" ||
		!strings.Contains(read, "saved as #1 (no description yet: add one with update)") || !strings.Contains(read, "#1 (priority 6, lines 3-6, updated by executor)") {
		t.Fatalf("read:\n%s", read)
	}
	if listed := f.call(`{"action":"list","task":"T-15"}`); !strings.Contains(listed, "undescribed: true") || !strings.Contains(listed, "priority: 6") {
		t.Fatalf("list of an undescribed entry:\n%s", listed)
	}
	if got := f.call(`{"action":"get","task":"T-15"}`); !strings.Contains(got, "(no description yet: add one with update)") {
		t.Fatalf("get of an undescribed entry:\n%s", got)
	}

	// The same range again, and lines inside it: printed, not saved again.
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`); !strings.Contains(got, "already saved as #1") || codeBlock(t, got, "r.go") != "r3\nr4\nr5\nr6\n" {
		t.Fatalf("read of a saved range:\n%s", got)
	}
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":4,"end":5}`); !strings.Contains(got, "these lines are inside entry #1 (lines 3-6)") || codeBlock(t, got, "r.go") != "r4\nr5\n" {
		t.Fatalf("read inside a saved range:\n%s", got)
	}
	// A description on the same range describes the entry.
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6,"description":"the loop","priority":2}`); !strings.Contains(got, "entry #1 updated") || !strings.Contains(got, "#1 (priority 2, lines 3-6") || !strings.Contains(got, "  the loop\n") {
		t.Fatalf("read with a description:\n%s", got)
	}

	// Reading the exact range of a stale entry refreshes it.
	f.write("r.go", strings.Replace(numbered("r", 20), "r4\n", "r4 edited\n", 1))
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":3,"end":6}`); !strings.Contains(got, "entry #1 held other code; it now holds the code below") || codeBlock(t, got, "r.go") != "r3\nr4 edited\nr5\nr6\n" {
		t.Fatalf("read of a stale entry's range:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-15"}`); strings.Contains(got, "stale") {
		t.Fatalf("the refreshed entry is still stale:\n%s", got)
	}

	// Beyond max_entries the code is still printed.
	f.call(`{"action":"read","task":"T-15","path":"r.go","start":10,"end":11}`)
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":15,"end":16}`); !strings.Contains(got, "not saved: the task already has 2 entries (max_entries)") || codeBlock(t, got, "r.go") != "r15\nr16\n" || strings.Contains(got, "#3") {
		t.Fatalf("read beyond max_entries:\n%s", got)
	}
	if got := f.call(`{"action":"read","task":"T-15","path":"r.go","start":30,"end":31}`); !strings.Contains(got, "error_code: range_outside_file") {
		t.Fatalf("read outside the file: %s", got)
	}

	listed := f.call(`{"action":"list","task":"T-15"}`)
	for _, want := range []string{"  reads: 7\n", "  reads_already_saved: 2\n", "  gets: 2\n"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("stats lack %q:\n%s", want, listed)
		}
	}
}

func TestCodeContextFindAndReuseAcrossTasks(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("glob_match.go", numbered("g", 10))
	f.write("other.go", numbered("o", 10))
	f.call(`{"action":"add","task":"T-20","path":"glob_match.go","start":2,"end":4,"priority":1,"description":"expandGlobBraces caps the brace expansion at 64 patterns."}`)
	f.call(`{"action":"add","task":"T-20","path":"other.go","start":1,"end":2,"priority":3,"description":"Glob options reach the walker here."}`)
	f.call(`{"action":"add","task":"T-20","path":"other.go","start":5,"end":6,"priority":3,"description":"Unrelated, soon edited."}`)

	got := f.call(`{"action":"find","task":"T-20","query":"glob cap."}`)
	if !strings.Contains(got, ": 1 entries match every word") || !strings.Contains(got, "- #1  glob_match.go 2-4  (priority 1)\n  expandGlobBraces caps") || strings.Contains(got, "#2") {
		t.Fatalf("find every word:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"walker brace"}`); !strings.Contains(got, "no entry matches every word; 2 match some") {
		t.Fatalf("find some words:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"glob_match.go"}`); !strings.Contains(got, "- #1  glob_match.go") {
		t.Fatalf("find by file name:\n%s", got)
	}
	if got := f.call(`{"action":"find","task":"T-20","query":"nothing"}`); !strings.Contains(got, "no saved entry matches") {
		t.Fatalf("find without a match:\n%s", got)
	}

	// Another task finds T-20's entries only with all_tasks, and only while current.
	f.write("other.go", strings.Replace(numbered("o", 10), "o5\n", "o5 edited\n", 1))
	if got := f.call(`{"action":"find","task":"T-21","query":"glob"}`); !strings.Contains(got, "no saved entry matches") {
		t.Fatalf("find without all_tasks reached another task:\n%s", got)
	}
	cross := f.call(`{"action":"find","task":"T-21","query":"glob","all_tasks":true}`)
	if !strings.Contains(cross, "in task T-21 and 1 other tasks: 2 entries match every word") ||
		!strings.Contains(cross, `- task T-20 #1  glob_match.go 2-4  (priority 1): copy it with action add, from_task "T-20", id 1`) {
		t.Fatalf("find across tasks:\n%s", cross)
	}
	if got := f.call(`{"action":"find","task":"T-21","query":"unrelated","all_tasks":true}`); !strings.Contains(got, "1 matches of other tasks left out: their code changed") {
		t.Fatalf("a stale entry of another task was offered:\n%s", got)
	}

	// add from_task copies the range, description and priority after checking the code.
	copied := f.call(`{"action":"add","task":"T-21","from_task":"T-20","id":1}`)
	if !strings.Contains(copied, "# copied from task T-20 #1") || !strings.Contains(copied, `command: read("glob_match.go", 2, 4)`) || !strings.Contains(copied, "expandGlobBraces caps") || !strings.Contains(copied, "priority: 1") {
		t.Fatalf("add from_task:\n%s", copied)
	}
	if got := f.call(`{"action":"add","task":"T-21","from_task":"T-20","id":3}`); !strings.Contains(got, "error_code: stale_source") {
		t.Fatalf("add from a stale entry: %s", got)
	}
	if got := f.call(`{"action":"add","task":"T-21","from_task":"T-21","id":1}`); !strings.Contains(got, "error_code: invalid_from_task") {
		t.Fatalf("add from the same task: %s", got)
	}
	if got := f.call(`{"action":"add","task":"T-21","from_task":"T-20","id":9}`); !strings.Contains(got, "error_code: entry_not_found") {
		t.Fatalf("add from a missing entry: %s", got)
	}

	// read takes the description of another task's entry with the same code.
	borrowed := f.call(`{"action":"read","task":"T-22","path":"other.go","start":1,"end":2}`)
	if !strings.Contains(borrowed, "saved as #1 (description copied from task T-20 #2, whose code is the same)") || !strings.Contains(borrowed, "#1 (priority 3, lines 1-2") || !strings.Contains(borrowed, "  Glob options reach the walker here.\n") {
		t.Fatalf("read did not reuse the description:\n%s", borrowed)
	}

	// Lines already saved are not saved again, so no description is said to be copied.
	// (T-20 #2 holds other.go 1-2, so its description could be borrowed.)
	f.call(`{"action":"read","task":"T-23","path":"other.go","start":1,"end":3}`)
	if got := f.call(`{"action":"read","task":"T-23","path":"other.go","start":1,"end":2}`); !strings.Contains(got, "these lines are inside entry #1 (lines 1-3)") || strings.Contains(got, "copied") {
		t.Fatalf("read inside a saved range claimed a copied description:\n%s", got)
	}

	listed := f.call(`{"action":"list","task":"T-20"}`)
	if !strings.Contains(listed, "  finds: 4\n") || !strings.Contains(listed, "  finds_without_match: 1\n") {
		t.Fatalf("find stats:\n%s", listed)
	}
}

func TestCodeContextOtherTasksSkipStateAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	store := codeContextStore{dir: dir}
	for _, name := range []string{"T-1.json", "T-2.json", "notes.txt", ".hidden.json", "T-3.json.tmp-1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".state"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := store.otherTasks("T-1", 10)
	if err != nil || len(files) != 1 || files[0].task != "T-2" {
		t.Fatalf("otherTasks = %+v, %v; want only T-2", files, err)
	}
	if files, _ := store.otherTasks("T-9", 1); len(files) != 1 {
		t.Fatalf("otherTasks limit = %+v", files)
	}
}
