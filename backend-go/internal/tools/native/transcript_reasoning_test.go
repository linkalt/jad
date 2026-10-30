package native

import (
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// reasoningFixture is the select fixture with a reasoning step in the other session's transcript.
func reasoningFixture(t *testing.T) *selectFixture {
	t.Helper()
	f := newSelectFixture(t)
	foreign := f.foreign
	reasoning := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "private plan"}}}
	writeTestTranscript(t, foreign, userLine("foreign question"), answerLine(1, 1, reasoning, textItem("foreign answer")))
	return f
}

func TestTranscriptOtherSessionReasoningIsHiddenByDefault(t *testing.T) {
	f := reasoningFixture(t)
	settings := transcriptTestSettings()
	if settings.AllowOtherReasoning {
		t.Fatal("allow_other_reasoning must default to false")
	}
	out := f.run(t, settings, map[string]any{"path": "profile:elsewhere/dev/coder", "step": "1.1", "part": "reasoning"}, tools.ExecutionContext{})
	if !strings.Contains(out, `"status":"forbidden"`) || !strings.Contains(out, "reasoning_denied") || !strings.Contains(out, "allow_other_reasoning is false") {
		t.Fatalf("other session reasoning must be refused by default: %s", out)
	}
	if strings.Contains(out, "private plan") {
		t.Fatalf("refused reasoning leaked its text: %s", out)
	}
}

func TestTranscriptOtherSessionReasoningIsShownWhenEnabled(t *testing.T) {
	f := reasoningFixture(t)
	settings := transcriptTestSettings()
	settings.AllowOtherReasoning = true
	out := f.run(t, settings, map[string]any{"path": "profile:elsewhere/dev/coder", "step": "1.1", "part": "reasoning"}, tools.ExecutionContext{})
	if !strings.Contains(out, "private plan") || strings.Contains(out, "reasoning_denied") {
		t.Fatalf("enabled reasoning must be shown: %s", out)
	}
}

func TestTranscriptOtherSessionMessagesStayReadableWithReasoningHidden(t *testing.T) {
	f := reasoningFixture(t)
	out := f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:elsewhere/dev/coder", "step": "1.2", "part": "both"}, tools.ExecutionContext{})
	if strings.Contains(out, "reasoning_denied") || !strings.Contains(out, "foreign answer") {
		t.Fatalf("a non-reasoning read of another session must not change: %s", out)
	}
}

func TestTranscriptOwnAndOtherProfileReasoningIsNotGated(t *testing.T) {
	f := reasoningFixture(t)
	own := f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:dev/coder", "step": "1.1", "part": "reasoning"}, tools.ExecutionContext{})
	if strings.Contains(own, "reasoning_denied") {
		t.Fatalf("the caller's own reasoning must not be gated: %s", own)
	}
	// review/reviewer is a profile of the caller's session, so the new key does not apply to it.
	sameSession := filepath.Join(f.dir, "same-session.jsonl")
	writeTestTranscript(t, sameSession, userLine("review question"), answerLine(1, 1, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "private plan"}}}, textItem("review answer")))
	f.profiles.targets["|review/reviewer"] = TranscriptTarget{Session: "demo", Profile: "review/reviewer", Files: TranscriptFiles{Messages: sameSession}}
	other := f.run(t, transcriptTestSettings(), map[string]any{"path": "profile:review/reviewer", "step": "1.1", "part": "reasoning"}, tools.ExecutionContext{})
	if strings.Contains(other, "reasoning_denied") || !strings.Contains(other, "private plan") {
		t.Fatalf("another profile of the same session must keep its reasoning: %s", other)
	}
}
