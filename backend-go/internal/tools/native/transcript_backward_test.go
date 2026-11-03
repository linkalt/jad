package native

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// equivalenceTranscript writes a transcript that exercises what a backward read could get
// wrong: results in another turn than their call (after the next user message, and before their
// own call's turn ends), calls that never get a result, a result whose call is missing, reasoning
// and unknown items, unreadable and over-long lines, a batch of user messages that opens one
// turn, and session_turn numbers that start above 1 as after a compaction.
func equivalenceTranscript(t *testing.T, seed uint64, turns int) string {
	t.Helper()
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	tools := []string{"read", "grep", "exec", "edit", "bash"}
	var lines []string
	var carried []string
	for turn := 1; turn <= turns; turn++ {
		number := turn + 40
		batch := func(text string, index int) string {
			line := userLine(text)
			return strings.Replace(line, `"__agent_session_input_batch_index":0`, fmt.Sprintf(`"__agent_session_input_batch_index":%d`, index), 1)
		}
		lines = append(lines, batch(fmt.Sprintf("question %d %s", number, strings.Repeat("word ", random.IntN(60))), 0))
		if random.IntN(4) == 0 {
			lines = append(lines, batch("and a second message of the same batch", 1))
		}
		for _, id := range carried {
			lines = append(lines, resultLine(id, `{"status":"ok"}`+"\nlate result of "+id))
		}
		carried = nil
		steps := 1 + random.IntN(7)
		for step := 1; step <= steps; step++ {
			id := fmt.Sprintf("c%d_%d", number, step)
			switch random.IntN(9) {
			case 0:
				lines = append(lines, answerLine(number, step, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "thinking"}}}, callItem(id, tools[random.IntN(len(tools))], `{"path":"a.go"}`)))
				lines = append(lines, resultLine(id, `{"status":"error"}`+"\nfailed"))
			case 1:
				lines = append(lines, answerLine(number, step, callItem(id, tools[random.IntN(len(tools))], `{"command":"sleep"}`)))
				carried = append(carried, id) // its result comes in the next turn
			case 2:
				lines = append(lines, answerLine(number, step, callItem(id, "exec", `{"code":"x"}`))) // never answered
			case 3:
				lines = append(lines, resultLine("orphan_"+id, "a result without its call"))
			case 4:
				lines = append(lines, answerLine(number, step, map[string]any{"type": "web_search_call"}))
			case 5:
				lines = append(lines, "{not json")
			case 6:
				lines = append(lines, `{"type":"function_call_output","call_id":"long","output":"`+strings.Repeat("x", 3000)+`"}`)
			default:
				lines = append(lines, answerLine(number, step, callItem(id, tools[random.IntN(len(tools))], fmt.Sprintf(`{"path":"f%d.go"}`, step))))
				lines = append(lines, resultLine(id, `{"status":"ok"}`+"\n"+strings.Repeat("out ", random.IntN(200))))
			}
		}
		lines = append(lines, answerLine(number, steps+1, textItem(fmt.Sprintf("answer %d", number))))
	}
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	writeTestTranscript(t, path, lines...)
	return path
}

// contentToken is the per-result random token of a CONTENT block, the only part of two renders
// of the same page that may differ.
var contentToken = regexp.MustCompile(`token=r_[A-Za-z0-9_-]+`)

// listPages lists with arguments and follows every cursor to the end, returning each page.
func listPages(t *testing.T, settings sessionfs.NativeTranscriptSettings, path string, arguments map[string]any) []string {
	t.Helper()
	var pages []string
	for range 1000 {
		out := runTranscript(t, settings, path, arguments)
		pages = append(pages, contentToken.ReplaceAllString(out, "token=*"))
		cursor := cursorOf(out)
		if cursor == "" {
			return pages
		}
		arguments = map[string]any{"cursor": cursor}
	}
	t.Fatal("the cursor did not end")
	return nil
}

// TestTranscriptBackwardListMatchesAWholeRead is the backward read's contract: every page of
// every list without search, followed through its cursors, is byte for byte the page a read of
// the whole range gives, with windows small enough that the read widens many times.
func TestTranscriptBackwardListMatchesAWholeRead(t *testing.T) {
	defer func(window int64, backward bool) { transcriptListWindow, transcriptListBackward = window, backward }(transcriptListWindow, transcriptListBackward)
	listings := []map[string]any{
		{}, {"turns": "-3"}, {"turns": "45-52"}, {"turns": "60-"}, {"role": "tool"}, {"role": "user"},
		{"tool": "exec"}, {"tool": "e*"}, {"role": "assistant", "turns": "-20"},
	}
	compared := 0
	for seed := uint64(1); seed <= 3; seed++ {
		path := equivalenceTranscript(t, seed, 30)
		for _, limits := range []struct{ entries, bytes int64 }{{4, 64 * 1024}, {100, 600}} {
			settings := transcriptTestSettings()
			settings.MaxListEntries, settings.ResponseMaxBytes = limits.entries, limits.bytes
			settings.LineMaxBytes = 2048
			for _, window := range []int64{1, 3000} {
				for _, arguments := range listings {
					transcriptListBackward = false
					want := listPages(t, settings, path, arguments)
					transcriptListBackward, transcriptListWindow = true, window
					got := listPages(t, settings, path, arguments)
					if strings.Join(got, "\n=====\n") != strings.Join(want, "\n=====\n") {
						for index := range max(len(want), len(got)) {
							if index >= len(want) || index >= len(got) || want[index] != got[index] {
								w, g := "", ""
								if index < len(want) {
									w = want[index]
								}
								if index < len(got) {
									g = got[index]
								}
								t.Fatalf("seed %d, limits %+v, window %d, %v: page %d of the backward read differs\nwhole:\n%s\nbackward:\n%s", seed, limits, window, arguments, index, w, g)
							}
						}
					}
					compared += len(want)
				}
			}
		}
	}
	t.Logf("%d pages compared", compared)
}
