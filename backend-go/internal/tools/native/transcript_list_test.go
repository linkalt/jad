package native

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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
	byTail := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{"tail": json.Number("1")}))
	byRole := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{"turns": "1-2", "role": "assistant"}))
	if strings.Join(byTurn, "\n") != strings.Join(byTail, "\n") || byTurn[1] != "2.1 assistant: ok" {
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

func TestTranscriptCallStatusReadsTheResultEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, userLine("q"),
		answerLine(1, 1, callItem("a", "t", `{}`), callItem("b", "t", `{}`), callItem("c", "t", `{}`)),
		resultLine("a", envelope("ok", "x")), resultLine("b", envelope("timeout", "x")), resultLine("c", "plain text, no envelope"))
	got := stepLines(runTranscript(t, transcriptTestSettings(), path, map[string]any{}))
	if !strings.HasPrefix(got[1], "1.1 call t ok ") || !strings.HasPrefix(got[2], "1.2 call t error ") || !strings.HasPrefix(got[3], "1.3 call t unknown 23B ") {
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
	var all []string
	arguments := map[string]any{"turns": "1"}
	pages := 0
	for ; pages < 20; pages++ {
		out := runTranscript(t, settings, path, arguments)
		page := stepLines(out)
		if len(page) > 7 || len(page) == 0 {
			t.Fatalf("page %d holds %d steps", pages, len(page))
		}
		all = append(all, page...)
		cursor := cursorOf(out)
		if cursor == "" {
			break
		}
		arguments = map[string]any{"cursor": cursor}
	}
	if len(all) != 26 || pages != 3 {
		t.Fatalf("%d steps in %d pages", len(all), pages+1)
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
	if len(stepLines(out)) != 1 || !strings.Contains(out, "1.0 user: do the thing") || cursorOf(out) == "" {
		t.Fatalf("out = %s", out)
	}
}
