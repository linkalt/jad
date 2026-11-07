package native

import (
	"strings"
	"testing"
)

// The headers name the stale entries, so the caller needs no list to find them.
func TestCodeContextStaleIDsInHeaders(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	original := numbered("a", 20)
	f.write("a.go", original)
	f.write("b.go", numbered("b", 10))
	f.call(`{"action":"read","task":"T-50","items":[
		{"path":"a.go","start":2,"end":3,"description":"one"},{"path":"a.go","start":5,"end":6,"description":"two"},
		{"path":"a.go","start":8,"end":9,"description":"three"},{"path":"b.go","start":1,"end":2,"description":"four"}]}`)
	edited := strings.NewReplacer("a2\n", "a2 edited\n", "a5\n", "a5 edited\n").Replace(original)
	f.write("a.go", edited)
	f.write("b.go", strings.Replace(numbered("b", 10), "b1\n", "b1 edited\n", 1))

	// get and list: a run of ids is one span.
	if got := f.call(`{"action":"get","task":"T-50"}`); !strings.Contains(got, ": 1 entries shown, 3 stale (#1-2, #4)\n") {
		t.Fatalf("get header without the stale ids:\n%s", got)
	}
	if got := f.call(`{"action":"list","task":"T-50"}`); !strings.Contains(got, ": 4 entries, 3 stale (#1-2, #4)\n") {
		t.Fatalf("list header without the stale ids:\n%s", got)
	}

	// read reports the stale entries of the file it read, never those of another file.
	if got := f.call(`{"action":"read","task":"T-50","path":"a.go","start":15,"end":16,"description":"five"}`); !strings.HasPrefix(got, "code_context task T-50, generation 2: #5 saved; stale in the files read: #1-2 (get them with ids for why)\n") {
		t.Fatalf("read of a.go did not name its stale entries:\n%s", got)
	}
	// update refreshes #1, so only #2 is left in a.go; a batch over both files names #4 too.
	if got := f.call(`{"action":"update","task":"T-50","items":[{"id":1},{"id":4,"description":"four, read again","start":3,"end":4}]}`); !strings.HasPrefix(got, "code_context task T-50, generation 3: 2 of 2 items done; stale in the files read: #2 (get them with ids for why)\n") {
		t.Fatalf("update did not name the entries still stale:\n%s", got)
	}
	f.write("b.go", strings.Replace(numbered("b", 10), "b3\n", "b3 edited\n", 1))
	if got := f.call(`{"action":"read","task":"T-50","items":[{"path":"a.go","start":11,"end":11},{"path":"b.go","start":9,"end":9}]}`); !strings.Contains(got, "; stale in the files read: #2, #4 (get them with ids for why)\n") {
		t.Fatalf("a read of two files did not name the stale entries of both:\n%s", got)
	}

	// Code that only moved is not stale; nothing is reported.
	f.write("c.go", numbered("c", 6))
	f.call(`{"action":"read","task":"T-51","path":"c.go","start":2,"end":3,"description":"moves"}`)
	f.write("c.go", "top\n"+numbered("c", 6))
	if got := f.call(`{"action":"read","task":"T-51","path":"c.go","start":6,"end":6}`); strings.Contains(got, "stale") {
		t.Fatalf("a moved entry was reported stale:\n%s", got)
	}
}
