package cronexpr

import (
	"strings"
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	base := time.Date(2026, 11, 4, 10, 17, 42, 0, time.UTC) // a Wednesday
	for _, test := range []struct{ expression, want string }{
		{"* * * * *", "2026-11-04T10:18:00Z"},
		{"*/15 * * * *", "2026-11-04T10:30:00Z"},
		{"0 3 * * *", "2026-11-05T03:00:00Z"},
		{"@daily", "2026-11-05T00:00:00Z"},
		{"@hourly", "2026-11-04T11:00:00Z"},
		{"30 9 * * 1-5", "2026-11-05T09:30:00Z"},
		{"0 0 * * 0", "2026-11-08T00:00:00Z"},
		{"0 0 * * 7", "2026-11-08T00:00:00Z"},
		{"0 0 1 * *", "2026-12-01T00:00:00Z"},
		{"0 0 29 2 *", "2028-02-29T00:00:00Z"},
		{"5,10 10 * * *", "2026-11-05T10:05:00Z"},
		{"20-40/10 10 * * *", "2026-11-04T10:20:00Z"},
		{"10/20 * * * *", "2026-11-04T10:30:00Z"},
		// Both days restricted: either one matches (the 6th, or the next Monday the 9th).
		{"0 0 6 * 1", "2026-11-06T00:00:00Z"},
		{"0 0 13 * 1", "2026-11-09T00:00:00Z"},
		{"@yearly", "2027-01-01T00:00:00Z"},
	} {
		schedule, err := Parse(test.expression)
		if err != nil {
			t.Fatalf("%s: %v", test.expression, err)
		}
		if got := schedule.Next(base).Format(time.RFC3339); got != test.want {
			t.Errorf("%s: next = %s, want %s", test.expression, got, test.want)
		}
	}
}

func TestNextKeepsTheLocation(t *testing.T) {
	location := time.FixedZone("plus2", 2*3600)
	schedule, _ := Parse("0 3 * * *")
	got := schedule.Next(time.Date(2026, 11, 4, 3, 0, 0, 0, location))
	if want := time.Date(2026, 11, 5, 3, 0, 0, 0, location); !got.Equal(want) || got.Location() != location {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestNeverMatchingGivesZero(t *testing.T) {
	schedule, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("next = %v", got)
	}
}

func TestParseRefusals(t *testing.T) {
	for expression, want := range map[string]string{
		"":              "want 5 fields",
		"* * * *":       "want 5 fields",
		"60 * * * *":    "minute: 60 is outside 0-59",
		"* 24 * * *":    "hour: 24 is outside 0-23",
		"* * 0 * *":     "day of month: 0 is outside 1-31",
		"* * * 13 *":    "month: 13 is outside 1-12",
		"* * * * 8":     "day of week: 8 is outside 0-7",
		"*/0 * * * *":   "step \"0\" must be a positive number",
		"5-1 * * * *":   "runs backwards",
		"1,,2 * * * *":  "empty list item",
		"* * * JAN *":   "\"JAN\" is not a number",
		"@every 5m":     "unknown macro",
		"a-b * * * * *": "want 5 fields",
	} {
		if _, err := Parse(expression); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", expression, err, want)
		}
	}
}
