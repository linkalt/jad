package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCodeContextBatchAddRunsEveryItem(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("b.go", numbered("b", 10))
	// path and priority given once apply to every item; an item's own field wins.
	got := f.call(`{"action":"add","task":"T-30","path":"b.go","priority":2,"items":[
		{"start":1,"end":2,"description":"first"},
		{"start":20,"end":21,"description":"outside the file"},
		{"start":4,"end":5,"description":"second","priority":1},
		{"start":1,"end":2,"description":"first again"}]}`)
	for _, want := range []string{
		"# code_context add: 3 of 4 items done\n",
		"# item 1: #1 saved\n",
		"# item 2 failed (range_outside_file): b.go has 10 lines; the range is 20-21\n",
		"# item 3: #2 saved\n",
		"# item 4: #1 (the same range is already saved as #1; nothing was added)\n",
		"task: \"T-30\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("batch add answer lacks %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "{") || strings.Count(got, "task: ") != 1 || strings.Count(got, "  - id: ") != 2 {
		t.Fatalf("batch add is not one YAML document of the two saved entries:\n%s", got)
	}
	list := f.call(`{"action":"list","task":"T-30"}`)
	if !strings.Contains(list, "  - id: 1\n    priority: 2\n") || !strings.Contains(list, "  - id: 2\n    priority: 1\n") || strings.Contains(list, "id: 3") {
		t.Fatalf("saved entries are not the two good items with their priorities:\n%s", list)
	}
}

func TestCodeContextBatchUpdateAndMissingFields(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("u.go", numbered("u", 6))
	f.call(`{"action":"add","task":"T-31","path":"u.go","priority":3,"items":[{"start":1,"end":2,"description":"one"},{"start":3,"end":4,"description":"two"}]}`)
	got := f.call(`{"action":"update","task":"T-31","items":[{"id":1,"description":"one, described"},{"id":9,"description":"none"},{"id":2,"priority":1}]}`)
	for _, want := range []string{"2 of 3 items done", "# item 1: #1 updated\n", "# item 2 failed (entry_not_found): task T-31 has no entry 9\n", "# item 3: #2 updated\n", "one, described"} {
		if !strings.Contains(got, want) {
			t.Fatalf("batch update answer lacks %q:\n%s", want, got)
		}
	}
	// An add item without its description fails on its own, as the schema fails a
	// single call without one.
	got = f.call(`{"action":"add","task":"T-31","path":"u.go","priority":2,"items":[{"start":5,"end":6},{"start":5,"end":6,"description":"five"}]}`)
	if !strings.Contains(got, "# item 1 failed (missing_fields): an add item needs path, start, end, description and priority, or from_task and id\n") || !strings.Contains(got, "# item 2: #3 saved\n") {
		t.Fatalf("missing fields of one item were not reported on that item:\n%s", got)
	}
	if got := f.call(`{"action":"add","task":"T-31","path":"u.go","start":5,"end":6,"priority":2}`); !strings.Contains(got, "validation_failed") {
		t.Fatalf("a single add without description passed the schema:\n%s", got)
	}
}

func TestCodeContextBatchAllFailedIsAnError(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("e.go", "only\n")
	got := f.call(`{"action":"update","task":"T-32","items":[{"id":1,"description":"x"},{"id":2,"description":"y"}]}`)
	if !strings.HasPrefix(got, `{"id":"call_1","status":"not_found"`) || !strings.Contains(got, "error_code: all_items_failed") ||
		!strings.Contains(got, "item_1: entry_not_found: task T-32 has no entry 1") || !strings.Contains(got, "item_2: entry_not_found: task T-32 has no entry 2") {
		t.Fatalf("a batch with no item done is not a tool error naming each item:\n%s", got)
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
	if !strings.HasPrefix(got, "code_context read: 3 of 4 items done\n") || codeBlock(t, got, "r1.go") != "aaaa\nbbbb\n" {
		t.Fatalf("batch read did not print the first item:\n%s", got)
	}
	// r2's 10 bytes no longer fit: it is saved, not printed. r3's 2 bytes do not fit either.
	r2 := got[strings.Index(got, "=== r2.go lines 1-2 ==="):]
	r2 = r2[:strings.Index(r2, "code_context task")]
	if strings.Contains(r2, "--- code ") || !strings.Contains(r2, "(code not printed: this call reached max_output_bytes; get it with ids [2])") {
		t.Fatalf("the item past max_output_bytes was printed or not named:\n%s", got)
	}
	if !strings.Contains(got, "code_context task T-33 item 4 failed (range_outside_file): r3.go has 1 lines; the range is 5-5\n") {
		t.Fatalf("the failed read item is not reported:\n%s", got)
	}
	list := f.call(`{"action":"list","task":"T-33"}`)
	if strings.Count(list, "  - id: ") != 3 || !strings.Contains(list, "  reads: 3\n") || !strings.Contains(list, "  code_bytes_printed: 10\n") {
		t.Fatalf("the reads were not all saved or the printed bytes are miscounted:\n%s", list)
	}
}

func TestCodeContextItemsShapeAndIDs(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("s.go", numbered("s", 4))
	f.call(`{"action":"add","task":"T-34","path":"s.go","priority":1,"items":[{"start":1,"end":1,"description":"a"},{"start":2,"end":2,"description":"b"},{"start":3,"end":3,"description":"c"}]}`)
	for _, arguments := range []string{
		`{"action":"get","task":"T-34","items":[{"path":"s.go"}]}`,
		`{"action":"add","task":"T-34","items":[{"start":1,"end":1,"query":"x"}]}`,
		`{"action":"add","task":"T-34","items":[]}`,
	} {
		if got := f.call(arguments); !strings.Contains(got, "validation_failed") {
			t.Fatalf("%s passed the schema:\n%s", arguments, got)
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
