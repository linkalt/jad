package native

import (
	"strings"
	"testing"
)

func TestCodeContextIDRanges(t *testing.T) {
	f := newCodeContextFixture(t, nil)
	f.write("r.go", numbered("r", 20))
	f.call(`{"action":"read","task":"T-40","path":"r.go","items":[
		{"start":1,"end":1,"description":"one"},{"start":3,"end":3,"description":"two"},{"start":5,"end":5,"description":"three"},
		{"start":7,"end":7,"description":"four"},{"start":9,"end":9,"description":"five"}]}`)
	f.call(`{"action":"drop","task":"T-40","ids":[2]}`)
	shown := func(got string) []string {
		var descriptions []string
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "#") {
				descriptions = append(descriptions, line[strings.LastIndex(line, "  ")+2:])
			}
		}
		return descriptions
	}
	for arguments, want := range map[string]string{
		// A range skips the ids no entry has; the gap of a dropped entry is not reported.
		`{"action":"get","task":"T-40","ids":["1-4"]}`: "one three four",
		// Ranges and ids mix, and spaces are allowed.
		`{"action":"get","task":"T-40","ids":[5," 1 - 1 "]}`: "one five",
		// A range alone, as ids or as id.
		`{"action":"get","task":"T-40","ids":"4-5"}`: "four five",
		`{"action":"get","task":"T-40","id":"3-9"}`:  "three four five",
	} {
		got := f.call(arguments)
		if descriptions := strings.Join(shown(got), " "); descriptions != want || strings.Contains(got, "no entry") {
			t.Fatalf("%s showed %q, want %q:\n%s", arguments, descriptions, want, got)
		}
	}
	if got := f.call(`{"action":"get","task":"T-40","ids":["10-20",2,1]}`); !strings.Contains(got, ": 1 entries shown\nno entry 2, 10-20\n") {
		t.Fatalf("ids and ranges naming no entry were not reported:\n%s", got)
	}
	if got := f.call(`{"action":"drop","task":"T-40","ids":["3-4","7-9"]}`); !strings.Contains(got, ": dropped 3, 4; no entry 7-9\n") {
		t.Fatalf("drop of id ranges:\n%s", got)
	}
	for arguments, want := range map[string]string{
		`{"action":"get","task":"T-40","ids":["5-2"]}`:                    "error_code: invalid_range",
		`{"action":"get","task":"T-40","ids":["0-3"]}`:                    "validation_failed",
		`{"action":"get","task":"T-40","ids":["a-b"]}`:                    "validation_failed",
		`{"action":"get","task":"T-40","ids":["99999999999999999999-1"]}`: "error_code: invalid_range",
	} {
		if got := f.call(arguments); !strings.Contains(got, want) {
			t.Fatalf("%s answered, want %s:\n%s", arguments, want, got)
		}
	}
}
