package native

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCodeContextGetPrintsARange(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("m.go", numbered("m", 30))
	f.write("o.go", numbered("o", 3))
	f.call(`{"action":"read","task":"T-40","path":"m.go","items":[
		{"start":2,"end":11,"priority":1,"description":"first"},
		{"start":20,"end":25,"priority":2,"description":"second"},
		{"start":10,"end":14,"priority":3,"description":"touching"}]}`)
	f.call(`{"action":"read","task":"T-40","path":"o.go","start":1,"end":3,"description":"other"}`)

	// Without a range get prints every entry whole.
	full := f.call(`{"action":"get","task":"T-40"}`)
	if !strings.HasPrefix(full, "code_context task T-40, generation 2: 4 entries shown\n") ||
		!strings.Contains(full, "=== m.go lines 2-14 ===\n") || !strings.Contains(full, "=== o.go lines 1-3 ===\n") || strings.Contains(full, " of ") {
		t.Fatalf("get without a range is not the whole task:\n%s", full)
	}

	// path and lines: only the entries holding those lines, cut to them.
	got := f.call(`{"action":"get","task":"T-40","path":"./m.go","start":5,"end":8}`)
	if !strings.HasPrefix(got, "code_context task T-40, generation 2: 1 entries shown in m.go lines 5-8\n") ||
		!strings.Contains(got, "=== m.go lines 5-8 of 2-11 ===\n#1 p1 lines 2-11  first\n") || codeBlock(t, got, "m.go") != "m5\nm6\nm7\nm8\n" ||
		strings.Contains(got, "o.go") || strings.Contains(got, "second") || strings.Contains(got, "touching") {
		t.Fatalf("get of path and lines:\n%s", got)
	}
	// An entry outside the lines is not merged in: 12-13 is only #3's.
	if got := f.call(`{"action":"get","task":"T-40","path":"m.go","start":12,"end":13}`); !strings.Contains(got, "=== m.go lines 12-13 of 10-14 ===\n#3 p3 lines 10-14  touching\n") ||
		codeBlock(t, got, "m.go") != "m12\nm13\n" || strings.Contains(got, "first") {
		t.Fatalf("get of lines only one entry holds:\n%s", got)
	}
	// ids with start only: from that line to the entry's end.
	if got := f.call(`{"action":"get","task":"T-40","id":2,"start":23}`); !strings.Contains(got, "1 entries shown lines 23-end\n") ||
		!strings.Contains(got, "=== m.go lines 23-25 of 20-25 ===\n") || codeBlock(t, got, "m.go") != "m23\nm24\nm25\n" {
		t.Fatalf("get of an id from a line:\n%s", got)
	}
	// path alone: that file's entries whole.
	if got := f.call(`{"action":"get","task":"T-40","path":"o.go"}`); !strings.HasPrefix(got, "code_context task T-40, generation 2: 1 entries shown in o.go\n") ||
		codeBlock(t, got, "o.go") != "o1\no2\no3\n" || strings.Contains(got, "m.go") {
		t.Fatalf("get of a path:\n%s", got)
	}
	// Lines no entry holds.
	if got := f.call(`{"action":"get","task":"T-40","path":"m.go","start":16,"end":18}`); !strings.Contains(got, ": 0 entries shown in m.go lines 16-18\nno entry in m.go holds lines 16-18\n") {
		t.Fatalf("get of lines no entry holds:\n%s", got)
	}
	for arguments, want := range map[string]string{
		`{"action":"get","task":"T-40","start":5}`:                       "get with start or end needs path or ids",
		`{"action":"get","task":"T-40","path":"m.go","start":8,"end":5}`: "error_code: invalid_range",
	} {
		if got := f.call(arguments); !strings.Contains(got, want) {
			t.Fatalf("%s answered, want %s:\n%s", arguments, want, got)
		}
	}
}

func TestCodeContextGetRangeOfABlockLeftOut(t *testing.T) {
	f := newCodeContextFixture(t, func(settings *sessionfs.NativeCodeContextSettings) { settings.MaxOutputBytes = 300 })
	f.write("big.go", numbered("a long line of code ", 40))
	f.write("e.go", numbered("e", 4))
	f.call(`{"action":"read","task":"T-41","path":"big.go","start":1,"end":40,"description":"big"}`)
	f.call(`{"action":"read","task":"T-41","path":"e.go","start":1,"end":4,"description":"edited"}`)
	got := f.call(`{"action":"get","task":"T-41"}`)
	if !strings.Contains(got, "=== left out by max_output_bytes: get them with ids, or part of one with ids, start and end ===\n#1 p6 big.go:1-40\n") {
		t.Fatalf("the left-out block does not point to a range:\n%s", got)
	}
	// Part of it fits.
	if got := f.call(`{"action":"get","task":"T-41","id":1,"start":1,"end":5}`); !strings.Contains(got, "=== big.go lines 1-5 of 1-40 ===\n") || strings.Contains(got, "left out") {
		t.Fatalf("part of the left-out block was not printed:\n%s", got)
	}
	// The lines are matched against an entry's range as relocated: e1-e4 moved to 2-5.
	f.write("e.go", "added\n"+numbered("e", 4))
	if got := f.call(`{"action":"get","task":"T-41","path":"e.go","start":4,"end":4}`); !strings.Contains(got, "1 entries shown in e.go lines 4-4, 1 relocated\n") ||
		!strings.Contains(got, "=== e.go lines 4-4 of 2-5 ===\n#2 p6 lines 2-5 [moved from 1-4]  edited\n") || codeBlock(t, got, "e.go") != "e3\n" {
		t.Fatalf("the lines were not matched against the relocated range:\n%s", got)
	}
	// A stale entry outside the lines is not listed, and not counted.
	f.write("e.go", "added\ne1\nchanged\ne3\ne4\n")
	if got := f.call(`{"action":"get","task":"T-41","path":"big.go","start":1,"end":2}`); strings.Contains(got, "stale") {
		t.Fatalf("a stale entry of another file was reported:\n%s", got)
	}
	if got := f.call(`{"action":"get","task":"T-41","path":"e.go","start":4,"end":4}`); !strings.Contains(got, "0 entries shown in e.go lines 4-4, 1 stale\n") || !strings.Contains(got, "=== 1 stale entries") {
		t.Fatalf("the stale entry holding the lines was not reported:\n%s", got)
	}
}
