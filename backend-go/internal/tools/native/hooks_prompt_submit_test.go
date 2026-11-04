package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
)

// A blocking command that fails names itself and stops the commands after it; a failing
// command that is not blocking does not. Context comes only from context commands that
// exit 0 and print something.
func TestFireRunEventResultBlocksAndCollectsContext(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "after")
	settings := hookTestSettings()
	settings.MaxPayloadBytes = 4096
	contextHook := runHook("notes", "echo '  first note  '")
	contextHook.Context = true
	failingContext := runHook("broken", "echo hidden; exit 2")
	failingContext.Context = true
	quiet := runHook("quiet", "true")
	quiet.Context = true
	plain := runHook("plain", "echo not context")
	settings.PromptSubmit = []sessionfs.RunEventHook{contextHook, failingContext, quiet, plain}

	result := FireRunEventResult(context.Background(), settings, "", RunEvent{Event: HookEventPromptSubmit, WorkspaceRoot: root, Prompt: "hi"})
	if result.BlockedBy != "" || len(result.Context) != 1 || result.Context[0] != (HookContext{Hook: "notes", Text: "first note"}) {
		t.Fatalf("result = %+v; want only the notes context and no block", result)
	}
	if text := PromptContextText(result.Context, 8192); text != "[prompt_submit hook notes]\nfirst note" {
		t.Fatalf("context text = %q", text)
	}

	gate := runHook("gate", "exit 3")
	gate.Blocking = true
	settings.PromptSubmit = []sessionfs.RunEventHook{contextHook, gate, runHook("after", "touch "+marker)}
	result = FireRunEventResult(context.Background(), settings, "", RunEvent{Event: HookEventPromptSubmit, WorkspaceRoot: root})
	if result.BlockedBy != "gate" || len(result.Context) != 1 {
		t.Fatalf("result = %+v; want a block by gate after the notes context", result)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command after the blocking one ran")
	}

	// A blocking command that passes blocks nothing, and blocking means the same for pre_compact.
	gate.Argv = []string{"sh", "-c", "exit 0"}
	settings.PreCompact = []sessionfs.RunEventHook{gate}
	if result := FireRunEventResult(context.Background(), settings, "", RunEvent{Event: HookEventPreCompact, WorkspaceRoot: root}); result.BlockedBy != "" {
		t.Fatalf("a passing blocking command blocked: %+v", result)
	}
	gate.Argv = []string{"sh", "-c", "exit 1"}
	settings.PreCompact = []sessionfs.RunEventHook{gate}
	if result := FireRunEventResult(context.Background(), settings, "", RunEvent{Event: HookEventPreCompact, WorkspaceRoot: root}); result.BlockedBy != "gate" {
		t.Fatalf("a failing blocking pre_compact did not block: %+v", result)
	}
}

// The prompt, the answer and the error are cut so the whole event fits the payload budget,
// on a character boundary, with the cut flags set; a small event is sent whole.
func TestRunEventPayloadFitsTheBudget(t *testing.T) {
	small := encodeRunEvent(RunEvent{Event: HookEventSubagentStop, Agent: "explore", Answer: "done", Outcome: RunOutcomeOK}, 1024)
	if !strings.Contains(small, `"answer":"done"`) || strings.Contains(small, "_cut") {
		t.Fatalf("small event = %s", small)
	}
	long := strings.Repeat("é<", 2000)
	encoded := encodeRunEvent(RunEvent{Event: HookEventSubagentStop, Prompt: long, Answer: long, Error: long}, 1024)
	if len(encoded) > 1024 {
		t.Fatalf("event is %d bytes, over the 1024 budget", len(encoded))
	}
	var decoded RunEvent
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.PromptCut || !decoded.AnswerCut || !decoded.ErrorCut {
		t.Fatalf("cut flags = %v %v %v", decoded.PromptCut, decoded.AnswerCut, decoded.ErrorCut)
	}
	for _, field := range []string{decoded.Prompt, decoded.Answer, decoded.Error} {
		if !utf8.ValidString(field) || !strings.HasPrefix(long, field) {
			t.Fatalf("a field was not cut on a character boundary: %q", field)
		}
	}
}

// subagent_stop runs only its own list.
func TestFireRunEventPicksTheSubagentStopList(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "stop.json")
	settings := hookTestSettings()
	settings.MaxPayloadBytes = 4096
	settings.SubagentStop = []sessionfs.RunEventHook{runHook("done", "cat > "+out)}
	if reports := FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventRunEnd, WorkspaceRoot: root}); len(reports) != 0 {
		t.Fatalf("run_end ran subagent_stop: %q", reports)
	}
	FireRunEvent(context.Background(), settings, "", RunEvent{Event: HookEventSubagentStop, WorkspaceRoot: root, Agent: "explore", Rounds: 2, Outcome: RunOutcomeOK})
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), `"agent":"explore"`) || !strings.Contains(string(data), `"rounds":2`) {
		t.Fatalf("payload = %s, %v", data, err)
	}
}

func TestHookBlockedErrorMessages(t *testing.T) {
	prompt := &HookBlockedError{Event: HookEventPromptSubmit, Hook: "gate", Report: "secret output"}
	if got := prompt.ClientMessage(); got != "The prompt_submit hook gate blocked the prompt; nothing was stored." {
		t.Fatalf("client message = %q", got)
	}
	compact := &HookBlockedError{Event: HookEventPreCompact, Hook: "keep"}
	if got := compact.ClientMessage(); got != "The pre_compact hook keep blocked the compaction." {
		t.Fatalf("client message = %q", got)
	}
}
