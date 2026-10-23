package tools

import "testing"

func TestCutTextNeverSplitsACharacter(t *testing.T) {
	if got, cut := CutText("hello", 10); got != "hello" || cut {
		t.Fatalf("short text = %q, cut %v", got, cut)
	}
	if got, cut := CutText("hello", 5); got != "hello" || cut {
		t.Fatalf("exact text = %q, cut %v", got, cut)
	}
	// "é" is two bytes; a cut in the middle must step back to the lead byte.
	if got, cut := CutText("aé", 2); got != "a" || !cut {
		t.Fatalf("split character = %q, cut %v", got, cut)
	}
	if got, cut := CutText("日本語", 4); got != "日" || !cut {
		t.Fatalf("three-byte characters = %q, cut %v", got, cut)
	}
	if got, cut := CutText("é", 0); got != "" || !cut {
		t.Fatalf("zero bytes = %q, cut %v", got, cut)
	}
}
