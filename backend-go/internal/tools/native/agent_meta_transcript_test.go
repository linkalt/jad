package native

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The transcript tool (and so the remote transcript route, which returns its output) renders
// a user record with __agent_meta exactly like the record without it: the field and its
// stored time are never shown to a model.
func TestTranscriptDoesNotShowAgentMeta(t *testing.T) {
	meta := `,"__agent_meta":{"stored_at":"2026-11-13T07:01:02.345Z"}`
	write := func(withMeta bool) string {
		path := filepath.Join(t.TempDir(), "messages.jsonl")
		first, second := userLine("first question"), userLine("second question")
		if withMeta {
			first = strings.TrimSuffix(first, "}") + meta + "}"
			second = strings.TrimSuffix(second, "}") + `,"__agent_meta":"malformed"}`
		}
		writeTestTranscript(t, path, first, answerLine(1, 1, textItem("first answer")), second, answerLine(2, 1, textItem("second answer")))
		return path
	}
	plain, marked := write(false), write(true)
	for _, arguments := range []map[string]any{{}, {"step": "1.0"}, {"step": "2.0"}, {"search": "question"}, {"turns": "1-2", "role": "user"}} {
		want := runTranscript(t, transcriptTestSettings(), plain, arguments)
		got := runTranscript(t, transcriptTestSettings(), marked, arguments)
		if strings.Contains(got, "__agent_meta") || strings.Contains(got, "stored_at") || strings.Contains(got, "2026-11-13") {
			t.Fatalf("%v: the tool showed the meta:\n%s", arguments, got)
		}
		token := regexp.MustCompile(`token=[A-Za-z0-9_-]+`)
		if token.ReplaceAllString(strings.ReplaceAll(got, marked, "<path>"), "") != token.ReplaceAllString(strings.ReplaceAll(want, plain, "<path>"), "") {
			t.Errorf("%v: output differs\n got: %s\nwant: %s", arguments, got, want)
		}
	}
}
