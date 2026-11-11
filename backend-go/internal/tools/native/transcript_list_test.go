package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func envelope(status, summary string) string {
	return `{"id":"x","status":"` + status + `","summary":"` + summary + `"}` + "\n\n--- INFORMATION ---\nk: v"
}

func stepLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 2 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line[:min(len(line), 8)], ".") {
			lines = append(lines, line)
		}
	}
	return lines
}

// pagedStepLines follows the cursors from the first call and returns the step lines of every page,
// the first page (the newest steps) first.
func pagedStepLines(t *testing.T, settings sessionfs.NativeTranscriptSettings, path string, arguments map[string]any) [][]string {
	t.Helper()
	var pages [][]string
	for range 200 {
		out := runTranscript(t, settings, path, arguments)
		pages = append(pages, stepLines(out))
		cursor := cursorOf(out)
		if cursor == "" {
			return pages
		}
		arguments = map[string]any{"cursor": cursor}
	}
	t.Fatal("the cursor did not end")
	return nil
}

// chronological joins pages (newest page first, each page oldest step first) into one list in file
// order, and fails when a page is not ascending or does not end before the page before it begins.
func chronological(t *testing.T, pages [][]string) []string {
	t.Helper()
	var all []string
	for index := len(pages) - 1; index >= 0; index-- {
		all = append(all, pages[index]...)
	}
	ascendingIDs(t, all)
	return all
}

func stepsTranscript(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	reasoning := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "secret thoughts"}}}
	writeTestTranscript(t, path,
		userLine("do the thing"),
		answerLine(1, 1, reasoning, callItem("c1", "read", `{"path":"a.go","limit":10}`), callItem("c2", "exec", `{"code":"false"}`)),
		resultLine("c1", envelope("ok", "read it")),
		resultLine("c2", envelope("failed", "exit 1")),
		answerLine(1, 2, textItem("all done"), callItem("c3", "grep", `{"pattern":"x"}`)),
		userLine("second"),
		answerLine(2, 1, textItem("ok"), reasoning),
	)
	return path
}

func TestTranscriptListNumbersStepsInFileOrderAndPairsACallWithItsResult(t *testing.T) {
	out := runTranscript(t, transcriptTestSettings(), stepsTranscript(t), map[string]any{"turns": "1-2"})
	got := stepLines(out)
	want := []string{
		"1.0 user: do the thing",
		"1.2 call read ok ",
		"1.3 call exec error ",
		"1.4 assistant: all done",
		"1.5 call grep no-result {\"pattern\":\"x\"}",
		"2.0 user: second",
		"2.1 assistant: ok",
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %q\n%s", got, out)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("line %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
	for _, line := range got {
		if strings.Contains(line, "result [") || strings.Contains(line, "thoughts") || strings.Contains(line, "reasoning") {
			t.Errorf("results and reasoning are not listed: %q", line)
		}
	}
	if !strings.Contains(got[1], "B {\"path\":\"a.go\",\"limit\":10}") || strings.Count(out, "\n1.2 ") != 1 {
		t.Errorf("a call line carries the result size and the arguments once: %q", got[1])
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "reasoning:") {
		t.Errorf("reasoning must appear nowhere: %s", out)
	}
	if !strings.Contains(out, "gaps are reasoning") {
		t.Errorf("the header explains the numbering: %s", out)
	}
}

func TestTranscriptStepNumbersAreTheSameWhateverTheSelection(t *testing.T) {
	path := stepsTranscript(t)
	byTurn := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "2"}))
	byTail := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "-1"}))
	byRole := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "1-2", "role": "assistant"}))
	if strings.Join(byTurn, "\n") != strings.Join(byTail, "\n") || byTurn[0] != "2.0 user: second" || byTurn[1] != "2.1 assistant: ok" {
		t.Fatalf("turn = %q, tail = %q", byTurn, byTail)
	}
	if len(byRole) != 2 || !strings.HasPrefix(byRole[0], "1.4 assistant") || !strings.HasPrefix(byRole[1], "2.1 assistant") {
		t.Fatalf("a filter keeps the numbers: %q", byRole)
	}
}

func TestTranscriptStepsFollowFileOrderAcrossACompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("kept"), answerLine(41, 1, textItem("a"), callItem("c", "read", `{}`)), resultLine("c", envelope("ok", "x")), answerLine(41, 2, textItem("b")))
	out := runTranscript(t, transcriptTestSettings(), path, map[string]any{})
	got := stepLines(out)
	if len(got) != 4 || !strings.HasPrefix(got[0], "41.0 user") || !strings.HasPrefix(got[1], "41.1 assistant") || !strings.HasPrefix(got[2], "41.2 call read ok") || !strings.HasPrefix(got[3], "41.3 assistant") {
		t.Fatalf("lines = %q", got)
	}
	if !strings.Contains(out, "compacted") {
		t.Fatalf("compaction note missing: %s", out)
	}
}

func TestTranscriptListsAnOrphanResultAndAnOrphanCallOnTheirOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"), resultLine("gone", envelope("ok", "late")), answerLine(1, 1, callItem("c", "read", `{}`)))
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	if len(got) != 3 || !strings.HasPrefix(got[1], "1.1 result [gone] ok") || !strings.Contains(got[1], "without its call") || !strings.HasPrefix(got[2], "1.2 call read no-result {}") {
		t.Fatalf("lines = %q", got)
	}
}

func TestTranscriptMarksUnknownItemsAndUnreadableLinesAsSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"), "{not json", answerLine(1, 1, map[string]any{"type": "web_search_call"}), `{"type":"compaction_marker"}`)
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	want := []string{"1.0 user: q", "1.1 [unreadable record]", "1.2 [web_search_call]", "1.3 [compaction_marker]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", got)
	}
}

func TestTranscriptCallStatusIsOkUnlessTheEnvelopeSaysOtherwise(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"),
		answerLine(1, 1, callItem("a", "t", `{}`), callItem("b", "t", `{}`), callItem("c", "t", `{}`)),
		resultLine("a", envelope("ok", "x")), resultLine("b", envelope("timeout", "x")), resultLine("c", "plain text, no envelope"))
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	if !strings.HasPrefix(got[1], "1.1 call t ok ") || !strings.HasPrefix(got[2], "1.2 call t error ") || !strings.HasPrefix(got[3], "1.3 call t ok 23B ") {
		t.Fatalf("lines = %q", got)
	}
}

func TestTranscriptKeepsArgumentKeysWholeAndCutsEachValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":"short","b":"` + strings.Repeat("x", 100) + `","c":[1,2,3],"d":null,"e":12}`, `{"a":"short","b":"xxxxxxxxxx`},
		{`{"list":[` + strings.Repeat(`"item",`, 40) + `1]}`, `{"list":["item","item"`},
		{`{"k":"héllo ✓ ` + strings.Repeat("日本語", 30) + `"}`, `{"k":"héllo ✓`},
		{`not json at all ` + strings.Repeat("y", 100), `not json at all`},
		{``, `{}`},
	}
	for _, tc := range cases {
		got := compactArguments(tc.in, 40)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q -> %q, want prefix %q", tc.in, got, tc.want)
		}
	}
	got := compactArguments(`{"a":"short","c":[1,2,3],"d":null,"e":12}`, 40)
	if got != `{"a":"short","c":[1,2,3],"d":null,"e":12}` {
		t.Errorf("values that fit stay whole: %s", got)
	}
}

func TestTranscriptSearchMatchesTheFullTextButListsOnlyThePreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	long := strings.Repeat("filler ", 100) + "HIDDENWORD" + strings.Repeat(" filler", 100)
	writeTestTranscript(t, path, userLine("q"), answerLine(1, 1, callItem("c", "read", `{"path":"a"}`), textItem(long)), resultLine("c", envelope("ok", "needle in result")))
	settings := transcriptTestSettings()
	settings.PreviewBytes = 40
	out := runTranscript(t, settings, path, map[string]any{"search": "hiddenword"})
	got := stepLines(out)
	if len(got) != 1 || !strings.HasPrefix(got[0], "1.2 assistant: filler") || strings.Contains(out, "HIDDENWORD") {
		t.Fatalf("lines = %q\n%s", got, out)
	}
	got = stepLines(runTranscript(t, settings, path, map[string]any{"search": "NEEDLE IN RESULT"}))
	if len(got) != 1 || !strings.HasPrefix(got[0], "1.1 call read ok") || strings.Contains(got[0], "needle") {
		t.Fatalf("a result match lists its call: %q", got)
	}
}

func TestTranscriptListPagesByStepWithStableNumbersAndMaxListEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	lines := []string{userLine("start")}
	for step := 1; step <= 25; step++ {
		lines = append(lines, answerLine(1, step, textItem(fmt.Sprintf("entry-%02d", step))))
	}
	writeTestTranscript(t, path, lines...)
	settings := transcriptTestSettings()
	settings.MaxListEntries = 7
	pages := pagedStepLines(t, settings, path, map[string]any{"turns": "1"})
	if len(pages) != 4 {
		t.Fatalf("%d pages: %q", len(pages), pages)
	}
	for index, want := range []int{7, 7, 7, 5} {
		if len(pages[index]) != want {
			t.Fatalf("page %d holds %d steps: %q", index, len(pages[index]), pages[index])
		}
	}
	// The first page is the newest steps, oldest first inside the page.
	if !strings.HasPrefix(pages[0][0], "1.19 ") || !strings.HasPrefix(pages[0][6], "1.25 ") {
		t.Fatalf("first page = %q", pages[0])
	}
	all := chronological(t, pages)
	if len(all) != 26 {
		t.Fatalf("%d steps", len(all))
	}
	for index, line := range all {
		if want := fmt.Sprintf("1.%d ", index); !strings.HasPrefix(line, want) {
			t.Fatalf("line %d = %q, want prefix %q", index, line, want)
		}
	}
}

func TestTranscriptListAlwaysHoldsOneStepWhateverTheBudget(t *testing.T) {
	settings := transcriptTestSettings()
	settings.ResponseMaxBytes = 60
	out := runTranscript(t, settings, stepsTranscript(t), map[string]any{"turns": "1"})
	if len(stepLines(out)) != 1 || !strings.Contains(out, "1.5 call grep") || cursorOf(out) == "" {
		t.Fatalf("out = %s", out)
	}
}

func TestTranscriptKeysStayWholeEvenWhenLong(t *testing.T) {
	got := compactArguments(`{"a_very_long_argument_name_that_exceeds_the_value_limit":"`+strings.Repeat("v", 100)+`"}`, 10)
	if !strings.HasPrefix(got, `{"a_very_long_argument_name_that_exceeds_the_value_limit":"`) {
		t.Fatalf("got %s", got)
	}
}

func TestTranscriptSearchMatchesACallsNameAndArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"), answerLine(1, 1, callItem("c", "exec", `{"code":"`+strings.Repeat("p", 300)+`NEEDLEARG"}`), callItem("d", "read", `{}`)))
	settings := transcriptTestSettings()
	settings.ValueBytes = 10
	got := stepLines(runTranscript(t, settings, path, map[string]any{"search": "needlearg"}))
	if len(got) != 1 || !strings.HasPrefix(got[0], "1.1 call exec") || strings.Contains(got[0], "NEEDLEARG") {
		t.Fatalf("lines = %q", got)
	}
	got = stepLines(runTranscript(t, settings, path, map[string]any{"search": "read"}))
	if len(got) != 1 || !strings.HasPrefix(got[0], "1.2 call read") {
		t.Fatalf("the tool name is searched too: %q", got)
	}
}

func ascendingIDs(t *testing.T, lines []string) {
	t.Helper()
	previous := transcriptStepID{}
	for _, line := range lines {
		id, _, _ := strings.Cut(line, " ")
		current, ok := parseTranscriptStepID(id)
		if !ok || !previous.less(current) {
			t.Fatalf("%q does not follow %v: the list must run from the oldest step up", line, previous)
		}
		previous = current
	}
}

func manyTurns(t *testing.T, turns, steps int) string {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	var lines []string
	for turn := 1; turn <= turns; turn++ {
		lines = append(lines, userLine(fmt.Sprintf("q%d", turn)))
		for step := 1; step <= steps; step++ {
			lines = append(lines, answerLine(turn, step, textItem(fmt.Sprintf("a%d-%d", turn, step))))
		}
	}
	writeTestTranscript(t, path, lines...)
	return path
}

func TestTranscriptListsOldestStepFirstAcrossTurns(t *testing.T) {
	got := stepLines(runTranscript(t, transcriptTestSettings(), manyTurns(t, 3, 4), map[string]any{}))
	if len(got) != 15 || !strings.HasPrefix(got[0], "1.0 ") || !strings.HasPrefix(got[5], "2.0 ") || !strings.HasPrefix(got[10], "3.0 ") || !strings.HasPrefix(got[14], "3.4 ") {
		t.Fatalf("lines = %q", got)
	}
	ascendingIDs(t, got)
}
func TestTranscriptPagesTowardOlderStepsAcrossTurnsWithoutGapsOrRepeats(t *testing.T) {
	path := manyTurns(t, 4, 6)
	settings := transcriptTestSettings()
	settings.MaxListEntries = 5
	pages := pagedStepLines(t, settings, path, map[string]any{})
	// Every page ascends, the first holds the newest steps, and each page ends where the one after it
	// (older) has to begin: joined oldest page first they are the whole transcript once.
	all := chronological(t, pages)
	if len(all) != 28 || len(pages) != 6 {
		t.Fatalf("%d steps in %d pages: %q", len(all), len(pages), all)
	}
	if !strings.HasPrefix(pages[0][len(pages[0])-1], "4.6 ") || !strings.HasPrefix(all[0], "1.0 ") || !strings.HasPrefix(all[27], "4.6 ") {
		t.Fatalf("first page %q, all %q", pages[0], all)
	}
	for index := 1; index < len(pages); index++ {
		last, _, _ := strings.Cut(pages[index][len(pages[index])-1], " ")
		first, _, _ := strings.Cut(pages[index-1][0], " ")
		a, _ := parseTranscriptStepID(last)
		b, _ := parseTranscriptStepID(first)
		if !a.less(b) {
			t.Fatalf("page %d ends at %s, not before the newer page's first %s", index, last, first)
		}
	}
}
func TestTranscriptCursorKeepsItsPlaceWhenTheFileGrowsBetweenPages(t *testing.T) {
	path := manyTurns(t, 2, 6)
	settings := transcriptTestSettings()
	settings.MaxListEntries = 4
	first := runTranscript(t, settings, path, map[string]any{})
	cursor := cursorOf(first)
	if cursor == "" {
		t.Fatalf("expected a cursor: %s", first)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{answerLine(2, 7, textItem("grown-1")), userLine("grown turn"), answerLine(3, 1, textItem("grown-2"))} {
		if _, err := file.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	pages := [][]string{stepLines(first)}
	arguments := map[string]any{"cursor": cursor}
	for page := 0; page < 30; page++ {
		out := runTranscript(t, settings, path, arguments)
		pages = append(pages, stepLines(out))
		if cursor = cursorOf(out); cursor == "" {
			break
		}
		arguments = map[string]any{"cursor": cursor}
	}
	all := chronological(t, pages)
	joined := strings.Join(all, "\n")
	if strings.Contains(joined, "grown") || len(all) != 14 {
		t.Fatalf("new steps must not appear nor shift the older pages: %d steps\n%s", len(all), joined)
	}
	if !strings.HasPrefix(all[13], "2.6 ") || !strings.HasPrefix(all[0], "1.0 ") {
		t.Fatalf("first %q, last %q", all[0], all[13])
	}
}

func TestTranscriptFiltersKeepTheOldestFirstOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path,
		userLine("needle one"), answerLine(1, 1, callItem("a", "read", `{"x":"needle"}`), textItem("needle reply")), resultLine("a", envelope("ok", "r")),
		userLine("plain"), answerLine(2, 1, callItem("b", "read", `{}`), callItem("c", "exec", `{"x":"needle"}`)), resultLine("b", envelope("ok", "r")), resultLine("c", envelope("ok", "r")),
		userLine("needle three"), answerLine(3, 1, textItem("done")))
	cases := []struct {
		arguments map[string]any
		want      []string
	}{
		{map[string]any{"role": "tool"}, []string{"1.1 ", "2.1 ", "2.2 "}},
		{map[string]any{"tool": "read"}, []string{"1.1 ", "2.1 "}},
		{map[string]any{"search": "needle"}, []string{"1.0 ", "1.1 ", "1.2 ", "2.2 ", "3.0 "}},
		{map[string]any{"search": "needle", "context": json.Number("1")}, []string{"1.0 ", "1.1 ", "1.2 ", "2.0 ", "2.1 ", "2.2 ", "3.0 ", "3.1 "}},
	}
	for _, tc := range cases {
		got := stepLines(runTranscript(t, transcriptTestSettings(), path, tc.arguments))
		if len(got) != len(tc.want) {
			t.Errorf("%v: lines = %q", tc.arguments, got)
			continue
		}
		for index, prefix := range tc.want {
			if !strings.HasPrefix(got[index], prefix) {
				t.Errorf("%v: line %d = %q, want prefix %q", tc.arguments, index, got[index], prefix)
			}
		}
	}
}

func TestTranscriptRejectsTailWithAHint(t *testing.T) {
	tool, err := NewTranscriptTool(transcriptTestSettings(), TranscriptFiles{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hint := tool.RejectedProperties["tail"]; !strings.Contains(hint, "turns -N") {
		t.Fatalf("hint = %q", hint)
	}
}

func partsResultLine(id string, texts ...string) string {
	parts := make([]any, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	raw, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": id, "output": parts})
	return string(raw)
}

func TestTranscriptCallStatusOfResultsWithoutAnEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"),
		answerLine(1, 1, callItem("a", "t", `{}`), callItem("b", "t", `{}`), callItem("c", "t", `{}`), callItem("d", "t", `{}`)),
		resultLine("a", ""), partsResultLine("b", "plain server text"),
		partsResultLine("c", `{"status":"error","server":"s"}`), partsResultLine("d", `{"status":"ok"}`))
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	for index, want := range []string{"1.1 call t ok 0B ", "1.2 call t ok 17B ", "1.3 call t error ", "1.4 call t ok "} {
		if !strings.HasPrefix(got[index+1], want) {
			t.Fatalf("line %d = %q, want prefix %q", index+1, got[index+1], want)
		}
	}
}

func TestTranscriptCallWithoutArgumentsFieldIsOk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	call := map[string]any{"type": "function_call", "call_id": "c", "name": "tool_name"}
	writeTestTranscript(t, path, userLine("q"), answerLine(1, 1, call), resultLine("c", envelope("ok", "done")))
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	if len(got) != 2 || !strings.HasPrefix(got[1], "1.1 call tool_name ok ") || !strings.HasSuffix(got[1], " {}") || strings.Contains(got[1], "unknown") {
		t.Fatalf("lines = %q", got)
	}
}
