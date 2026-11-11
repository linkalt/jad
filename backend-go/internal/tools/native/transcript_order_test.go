package native

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func longTranscript(t *testing.T, steps int, pad int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	lines := []string{userLine("start")}
	for step := 1; step <= steps; step++ {
		lines = append(lines, answerLine(1, step, textItem(fmt.Sprintf("entry-%02d %s", step, strings.Repeat("p", pad)))))
	}
	writeTestTranscript(t, path, lines...)
	return path
}

// A byte budget drops the oldest steps of the page and keeps the newest, and the lines that stay
// read oldest to newest; paging back never skips or repeats a step.
func TestTranscriptByteBudgetDropsTheHeadAndKeepsTheNewestTail(t *testing.T) {
	path := longTranscript(t, 30, 80)
	whole := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 400
	pages := pagedStepLines(t, settings, path, map[string]any{})
	if len(pages) < 4 {
		t.Fatalf("a 400 byte budget must split 31 steps into several pages, got %d", len(pages))
	}
	first := runTranscript(t, settings, path, map[string]any{})
	if !strings.Contains(first, "truncated: true") || cursorOf(first) == "" {
		t.Fatalf("a cut page says so and carries a cursor:\n%s", first)
	}
	if last := pages[0][len(pages[0])-1]; !strings.HasPrefix(last, "1.30 ") {
		t.Fatalf("the newest step stays at the end of the first page: %q", last)
	}
	if strings.HasPrefix(pages[0][0], "1.0 ") {
		t.Fatalf("the oldest step is the one dropped from the first page: %q", pages[0])
	}
	for index, page := range pages {
		if size := len(strings.Join(page, "\n")); size > 400 {
			t.Errorf("page %d holds %d bytes of steps", index, size)
		}
	}
	if joined := chronological(t, pages); strings.Join(joined, "\n") != strings.Join(whole, "\n") {
		t.Fatalf("pages joined oldest first differ from the unbounded list:\n%q\n%q", joined, whole)
	}
}

func TestTranscriptEntryLimitKeepsTheNewestStepsOldestFirst(t *testing.T) {
	path := longTranscript(t, 10, 1)
	settings := transcriptTestSettings()
	settings.MaxListEntries = 3
	out := runTranscript(t, settings, path, map[string]any{})
	got := stepLines(out)
	if len(got) != 3 || !strings.HasPrefix(got[0], "1.8 ") || !strings.HasPrefix(got[1], "1.9 ") || !strings.HasPrefix(got[2], "1.10 ") {
		t.Fatalf("lines = %q", got)
	}
	next := stepLines(runTranscript(t, settings, path, map[string]any{"cursor": cursorOf(out)}))
	if len(next) != 3 || !strings.HasPrefix(next[0], "1.5 ") || !strings.HasPrefix(next[2], "1.7 ") {
		t.Fatalf("the cursor continues just before the first page: %q", next)
	}
}

// A step line larger than the whole budget is shortened in its middle (the id and the end stay),
// and the cursor still advances to the next older step, so paging ends.
func TestTranscriptOversizedStepIsShortenedAndTheCursorStillAdvances(t *testing.T) {
	path := longTranscript(t, 6, 150)
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 70
	pages := pagedStepLines(t, settings, path, map[string]any{})
	if len(pages) != 7 {
		t.Fatalf("each oversized step is a page of its own: %d pages", len(pages))
	}
	seen := map[string]bool{}
	for index, page := range pages {
		short := index == len(pages)-1 // "1.0 user: start" is the one step that fits
		if len(page) != 1 || len(page[0]) > 70 || short == strings.Contains(page[0], "…+") {
			t.Fatalf("page %d = %q", index, page)
		}
		id, _, _ := strings.Cut(page[0], " ")
		if seen[id] {
			t.Fatalf("step %s repeated", id)
		}
		seen[id] = true
	}
	if !strings.HasPrefix(pages[0][0], "1.6 ") || !strings.HasPrefix(pages[6][0], "1.0 ") {
		t.Fatalf("first %q, last %q", pages[0][0], pages[6][0])
	}
	if out := runTranscript(t, settings, path, map[string]any{}); !strings.Contains(out, "truncated: true") {
		t.Fatalf("a shortened page is marked truncated:\n%s", out)
	}
}

func TestTranscriptHeaderAndDescriptionsNameTheNewOrder(t *testing.T) {
	out := runTranscript(t, transcriptTestSettings(), longTranscript(t, 2, 1), map[string]any{})
	if !strings.Contains(out, "oldest step first in the page") || strings.Contains(out, "newest step first") {
		t.Fatalf("header: %s", out)
	}
	for name, text := range map[string]string{"description": transcriptDescription, "tail hint": transcriptRejectedProperties["tail"], "schema": string(transcriptInputSchema)} {
		if strings.Contains(text, "newest step first") || strings.Contains(text, "newest first") || strings.Contains(text, "starts at the newest") {
			t.Errorf("%s still describes the old order: %s", name, text)
		}
	}
	if !strings.Contains(transcriptDescription, "oldest step first within the page") {
		t.Errorf("description: %s", transcriptDescription)
	}
}
