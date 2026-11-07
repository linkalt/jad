package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCodeContextBatchReadRunsEveryItemInOneWrite(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("b.go", numbered("b", 10))
	// path and priority given once apply to every item; an item's own field wins. With a
	// description each range is saved without printing its code.
	got := f.call(`{"action":"read","task":"T-30","path":"b.go","priority":2,"items":[
		{"start":1,"end":2,"description":"first"},
		{"start":20,"end":21,"description":"outside the file"},
		{"start":4,"end":5,"description":"second","priority":1},
		{"start":1,"end":2,"description":"first again"}]}`)
	want := "code_context task T-30, generation 1: 3 of 4 items done\n" +
		"item 1: #1 saved\n#1 p2 b.go:1-2  first\n" +
		"item 2 failed (range_outside_file): b.go has 10 lines; the range is 20-21\n" +
		"item 3: #2 saved\n#2 p1 b.go:4-5  second\n" +
		"item 4: #1 updated\n#1 p2 b.go:1-2  first again\n"
	if got != want {
		t.Fatalf("batch read answered:\n%s\nwant:\n%s", got, want)
	}
	if list := f.call(`{"action":"list","task":"T-30"}`); list != "code_context task T-30, generation 1: 2 entries\n#2 p1 b.go:4-5  second\n#1 p2 b.go:1-2  first again\n" {
		t.Fatalf("saved entries are not the two good items:\n%s", list)
	}
}

func TestCodeContextBatchUpdateAndMissingFields(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("u.go", numbered("u", 6))
	f.call(`{"action":"read","task":"T-31","path":"u.go","priority":3,"items":[{"start":1,"end":2,"description":"one"},{"start":3,"end":4,"description":"two"}]}`)
	got := f.call(`{"action":"update","task":"T-31","items":[{"id":1,"description":"one, described"},{"id":9,"description":"none"},{"id":2,"priority":1},{"priority":2}]}`)
	want := "code_context task T-31, generation 2: 2 of 4 items done\n" +
		"item 1: #1 updated\n#1 p3 u.go:1-2  one, described\n" +
		"item 2 failed (entry_not_found): task T-31 has no entry 9\n" +
		"item 3: #2 updated\n#2 p1 u.go:3-4  two\n" +
		"item 4 failed (missing_fields): update needs id\n"
	if got != want {
		t.Fatalf("batch update answered:\n%s\nwant:\n%s", got, want)
	}
	// A read item without its range fails on its own, as a single read without one does.
	got = f.call(`{"action":"read","task":"T-31","path":"u.go","items":[{"start":5},{"start":5,"end":6,"description":"five"}]}`)
	if !strings.Contains(got, "item 1 failed (missing_fields): read needs path, start and end\n") || !strings.Contains(got, "item 2: #3 saved\n") {
		t.Fatalf("missing fields of one item were not reported on that item:\n%s", got)
	}
	if got := f.call(`{"action":"read","task":"T-31","path":"u.go","start":5}`); !strings.Contains(got, "error_code: missing_fields") || !strings.Contains(got, "read needs path, start and end") {
		t.Fatalf("a single read without end was not refused:\n%s", got)
	}
}

func TestCodeContextBatchAllFailedWritesNothing(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("e.go", "only\n")
	got := f.call(`{"action":"update","task":"T-32","task_description":"never written","items":[{"id":1,"description":"x"},{"id":2,"description":"y"}]}`)
	if !strings.HasPrefix(got, `{"id":"call_1","status":"not_found"`) || !strings.Contains(got, "error_code: all_items_failed") ||
		!strings.Contains(got, "item_1: entry_not_found: task T-32 has no entry 1") || !strings.Contains(got, "item_2: entry_not_found: task T-32 has no entry 2") {
		t.Fatalf("a batch with no item done is not a tool error naming each item:\n%s", got)
	}
	// Not even the task description was written.
	if got := f.call(`{}`); !strings.HasPrefix(got, "code_context: no tasks yet") {
		t.Fatalf("a call whose every item failed wrote the task:\n%s", got)
	}
	small := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxBatchItems = 2 })
	if got := small.call(`{"action":"read","task":"T-32","path":"e.go","items":[{"start":1,"end":1},{"start":1,"end":1},{"start":1,"end":1}]}`); !strings.Contains(got, "error_code: too_many_items") || !strings.Contains(got, "max_batch_items is 2") {
		t.Fatalf("max_batch_items was not enforced:\n%s", got)
	}
}

func TestCodeContextBatchReadKeepsTheOutputBudget(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxOutputBytes = 10 })
	f.write("r1.go", "aaaa\nbbbb\n")
	f.write("r2.go", "cccc\ndddd\n")
	f.write("r3.go", "e\n")
	got := f.call(`{"action":"read","task":"T-33","items":[{"path":"r1.go","start":1,"end":2},{"path":"r2.go","start":1,"end":2},{"path":"r3.go","start":1,"end":1},{"path":"r3.go","start":5,"end":5}]}`)
	if !strings.HasPrefix(got, "code_context task T-33, generation 1: 3 of 4 items done\n") || codeBlock(t, got, "r1.go") != "aaaa\nbbbb\n" {
		t.Fatalf("batch read did not print the first item:\n%s", got)
	}
	// r2's 10 bytes no longer fit: it is saved, not printed. r3's 2 bytes do not fit either.
	r2 := got[strings.Index(got, "=== r2.go lines 1-2 ==="):]
	r2 = r2[:strings.Index(r2, "item 3")]
	if strings.Contains(r2, "--- code ") || !strings.Contains(r2, "(code not printed: this call reached max_output_bytes; get it with ids [2])") {
		t.Fatalf("the item past max_output_bytes was printed or not named:\n%s", got)
	}
	if !strings.Contains(got, "item 4 failed (range_outside_file): r3.go has 1 lines; the range is 5-5\n") {
		t.Fatalf("the failed read item is not reported:\n%s", got)
	}
	if list := f.call(`{"action":"list","task":"T-33"}`); strings.Count(list, "  (no description)\n") != 3 {
		t.Fatalf("the reads were not all saved:\n%s", list)
	}
}

func TestCodeContextItemsShapeAndIDs(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("s.go", numbered("s", 4))
	f.call(`{"action":"read","task":"T-34","path":"s.go","priority":1,"items":[{"start":1,"end":1,"description":"a"},{"start":2,"end":2,"description":"b"},{"start":3,"end":3,"description":"c"}]}`)
	for arguments, want := range map[string]string{
		`{"action":"get","task":"T-34","items":[{"path":"s.go"}]}`:                  "error_code: invalid_field",
		`{"action":"read","task":"T-34","items":[{"start":1,"end":1,"query":"x"}]}`: "validation_failed",
		`{"action":"read","task":"T-34","items":[]}`:                                "validation_failed",
	} {
		if got := f.call(arguments); !strings.Contains(got, want) {
			t.Fatalf("%s answered, want %s:\n%s", arguments, want, got)
		}
	}
	// drop and get take items that only name an id as ids.
	if got := f.call(`{"action":"drop","task":"T-34","items":[{"id":1},{"id":3}]}`); !strings.Contains(got, "dropped 1, 3") {
		t.Fatalf("drop with id items did not drop them:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-34","items":[{"id":2}]}`); codeBlock(t, got, "s.go") != "s2\n" {
		t.Fatalf("get with id items did not print that entry:\n%s", got)
	}
}
