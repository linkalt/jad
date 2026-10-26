package native

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMidCutLeavesTextThatFits(t *testing.T) {
	for _, text := range []string{"", "abc", strings.Repeat("x", 50)} {
		if got := midCut(text, 50); got != text {
			t.Errorf("midCut(%q) = %q", text, got)
		}
	}
}

func TestMidCutKeepsHeadAndTailAndCountsTheOmittedBytes(t *testing.T) {
	text := strings.Repeat("a", 100) + strings.Repeat("b", 100)
	got := midCut(text, 60)
	if len(got) > 60 || !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "bbbb") || !strings.Contains(got, "…+") {
		t.Fatalf("got %q (%d bytes)", got, len(got))
	}
	var omitted int
	left, right, _ := strings.Cut(got, "…+")
	if _, err := fmt.Sscanf(right, "%d", &omitted); err != nil {
		t.Fatal(err)
	}
	rest := right[strings.Index(right, "…")+len("…"):]
	if len(left)+omitted+len(rest) != len(text) {
		t.Fatalf("head %d + omitted %d + tail %d != %d", len(left), omitted, len(rest), len(text))
	}
	if len(left) <= len(rest) {
		t.Fatalf("head must be longer than tail: %q", got)
	}
}

func TestMidCutNeverSplitsARuneAndStaysInTheBudget(t *testing.T) {
	text := strings.Repeat("héllo wörld ✓ 日本語 😀 ", 40)
	for budget := 0; budget <= len(text)+2; budget++ {
		got := midCut(text, budget)
		if !utf8.ValidString(got) {
			t.Fatalf("budget %d: invalid UTF-8 %q", budget, got)
		}
		if len(got) > len(text) {
			t.Fatalf("budget %d: output longer than input", budget)
		}
		if budget >= len("…+999 bytes…")+2 && len(got) > budget {
			t.Fatalf("budget %d: %d bytes", budget, len(got))
		}
	}
}

func TestMidCutTinyBudgetsReturnTheBareMarkerOrTheText(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := midCut(long, 0); got != "…+500 bytes…" {
		t.Errorf("budget 0: %q", got)
	}
	if got := midCut(long, 5); got != "…+500 bytes…" {
		t.Errorf("budget 5: %q", got)
	}
	if got := midCut("abcdefghij", 3); got != "abcdefghij" {
		t.Errorf("a marker longer than the text must not replace it: %q", got)
	}
}

func TestMidCutBoundaryJustOverTheBudget(t *testing.T) {
	text := strings.Repeat("z", 31)
	got := midCut(text, 30)
	if len(got) > 31 || len(got) > len(text) {
		t.Fatalf("got %q", got)
	}
	if midCut(strings.Repeat("z", 30), 30) != strings.Repeat("z", 30) {
		t.Fatal("exact fit must be untouched")
	}
}

func TestParseTranscriptSteps(t *testing.T) {
	for _, tc := range []struct {
		in       string
		from, to string
		ok       bool
	}{
		{"3.2", "3.2", "3.2", true},
		{"3.0", "3.0", "3.0", true},
		{"3.1-3.4", "3.1", "3.4", true},
		{"3.4-4.1", "3.4", "4.1", true},
		{" 12.30 ", "12.30", "12.30", true},
		{"3", "", "", false},
		{"3.", "", "", false},
		{".2", "", "", false},
		{"0.1", "", "", false},
		{"3.-1", "", "", false},
		{"a.b", "", "", false},
		{"3.4-3.1", "", "", false},
		{"3.1-4", "", "", false},
		{"", "", "", false},
		{"3.1-3.2-3.3", "", "", false},
	} {
		from, to, ok := parseTranscriptSteps(tc.in)
		if ok != tc.ok || ok && (from.String() != tc.from || to.String() != tc.to) {
			t.Errorf("%q: %v %v %v", tc.in, from, to, ok)
		}
	}
}
