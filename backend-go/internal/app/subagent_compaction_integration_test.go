package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

const childStateBlockMarker = "STATE BLOCK MARKER 7731"

// childFile reads a file of the one child folder the run left behind.
func childFile(t *testing.T, f subagentFixture, name string) (string, bool) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.toolDir, ".subagents", "*", name))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 1 {
		t.Fatalf("%d child folders hold %s, want at most one: %v", len(matches), name, matches)
	}
	if len(matches) == 0 {
		return "", false
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// bigNotes makes every read of notes.txt large, so a compaction has something to drop.
func bigNotes(t *testing.T, f subagentFixture) {
	t.Helper()
	writeFile(t, filepath.Join(f.workspace, "notes.txt"), []byte(strings.Repeat("a long line of notes\n", 400)))
}

func childCompactionFixture(t *testing.T, agent, session map[string]any) subagentFixture {
	t.Helper()
	agent["model"] = "child.json"
	if session != nil {
		agent["__session"] = session
	}
	f := newSubagentFixture(t, agent)
	bigNotes(t, f)
	f.provider.childRounds = 3
	f.provider.childInputTokens = 5000
	return f
}

func runChildToTheEnd(t *testing.T, f subagentFixture) {
	t.Helper()
	body := f.run(t)
	if !strings.Contains(body, "event: completed") || !strings.Contains(body, "the answer is 42") {
		t.Fatalf("the child did not finish with its answer:\n%s", body)
	}
}

func TestChildCompactsAtItsOwnTriggerNotTheSessions(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"compaction_trigger_tokens": 100}, nil)
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); !ok {
		t.Fatal("the child never compacted, though every response was over its own trigger of 100 tokens")
	}
	transcript, ok := childFile(t, f, "messages.jsonl")
	if !ok || !strings.Contains(transcript, "the answer is 42") {
		t.Fatalf("the final answer is not in the child transcript, so a restart could not restore it:\n%s", transcript)
	}
}

func TestChildBelowItsTriggerKeepsItsFullTranscript(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"compaction_trigger_tokens": 100000}, map[string]any{"compaction": map[string]any{"trigger_tokens": 100}})
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); ok {
		t.Fatal("the child compacted at the session's trigger of 100 tokens, not its own of 100000")
	}
}

func TestChildInheritsTheSessionsTriggerWhenTheAgentSetsNone(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{}, map[string]any{"compaction": map[string]any{"trigger_tokens": 100}})
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); !ok {
		t.Fatal("the child did not compact at the session's trigger of 100 tokens")
	}
}

func TestChildDefaultTriggerLeavesItUncompactedUnderTheSessionDefault(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{}, nil)
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); ok {
		t.Fatal("the child compacted at 5000 tokens, under the session default trigger")
	}
}

func TestChildWithAutoCompactionOffNeverCompacts(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"auto_compaction": false}, map[string]any{"compaction": map[string]any{"trigger_tokens": 100}})
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); ok {
		t.Fatal("the child compacted though the agent turned auto_compaction off")
	}
}

func TestChildWithAutoCompactionOnUsesTheSessionTriggerEvenWhenTheSessionTurnedItOff(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"auto_compaction": true}, map[string]any{"compaction": map[string]any{"auto_enabled": false, "trigger_tokens": 100}})
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); !ok {
		t.Fatal("the child did not compact though the agent turned auto_compaction on")
	}
}

func TestParentSessionTranscriptIsNotCompactedByAChild(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"compaction_trigger_tokens": 100}, nil)
	runChildToTheEnd(t, f)
	if _, err := os.Stat(filepath.Join(f.stateDir, "compaction-cache.json")); err == nil {
		t.Fatal("the session's own compaction cache exists after a child compacted")
	}
}

// enableSessionStateContext turns the compaction trigger on for the session and stores one
// block in it, as a compaction of the session would have.
func enableSessionStateContext(t *testing.T, f subagentFixture, enabled bool) {
	t.Helper()
	settingPath := filepath.Join(f.sessionDir, "session.json")
	var session map[string]any
	data, err := os.ReadFile(settingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	setting := session["settings"].(map[string]any)
	setting["statecontext"] = map[string]any{"enabled": enabled, "role": "developer", "triggers": map[string]any{"compaction": map[string]any{"tools": []string{"subagent"}, "max_bytes": 8192}}}
	rewritten, _ := json.Marshal(session)
	writeFile(t, settingPath, rewritten)

	location, err := sessionfs.Resolve(f.config.Roots, "demo")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	older, _ := json.Marshal(map[string]any{"schema_version": 1, "generated_at": "2026-01-01T00:00:00Z", "trigger": "compaction", "text": "OLDER BLOCK 1111", "role": "developer", "after": 0})
	newest, _ := json.Marshal(map[string]any{"schema_version": 1, "generated_at": "2026-01-02T00:00:00Z", "trigger": "compaction", "text": childStateBlockMarker, "role": "developer", "after": 0})
	writeFile(t, config.StoragePaths.StateContext, []byte(string(older)+"\n"+string(newest)+"\n"))
}

func childRequestsWith(f subagentFixture, text string) (with, total int) {
	for _, request := range f.provider.forModel("child-model") {
		total++
		if strings.Contains(request.Text, text) {
			with++
		}
	}
	return with, total
}

func TestChildWithStateContextSeesTheNewestSessionBlockInEveryRequest(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"state_context": true}, nil)
	enableSessionStateContext(t, f, true)
	runChildToTheEnd(t, f)
	with, total := childRequestsWith(f, childStateBlockMarker)
	if total < 4 || with != total {
		t.Fatalf("%d of %d child requests carried the session's state-context block, want all of at least 4", with, total)
	}
	if older, _ := childRequestsWith(f, "OLDER BLOCK 1111"); older != 0 {
		t.Fatalf("%d child requests carried an older block, only the newest is sent", older)
	}
	parentWith := 0
	for _, request := range f.provider.forModel("parent-model") {
		if strings.Contains(request.Text, childStateBlockMarker) {
			parentWith++
		}
	}
	if parentWith == 0 {
		t.Fatal("the parent never saw the block, so the setup did not store it where the session reads it")
	}
}

func TestChildStillSeesTheBlockAfterItCompacts(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"state_context": true, "compaction_trigger_tokens": 100}, nil)
	enableSessionStateContext(t, f, true)
	runChildToTheEnd(t, f)
	if _, ok := childFile(t, f, "compaction-cache.json"); !ok {
		t.Fatal("the child never compacted")
	}
	with, total := childRequestsWith(f, childStateBlockMarker)
	if total < 4 || with != total {
		t.Fatalf("%d of %d child requests carried the block after a compaction, want all", with, total)
	}
}

func TestChildWithoutStateContextSeesNoBlock(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{}, nil)
	enableSessionStateContext(t, f, true)
	runChildToTheEnd(t, f)
	if with, _ := childRequestsWith(f, childStateBlockMarker); with != 0 {
		t.Fatalf("%d child requests carried the block though the agent did not ask for it", with)
	}
}

func TestChildAskingForStateContextGetsNoneWhenTheSessionHasItOff(t *testing.T) {
	f := childCompactionFixture(t, map[string]any{"state_context": true}, nil)
	enableSessionStateContext(t, f, false)
	runChildToTheEnd(t, f)
	if with, _ := childRequestsWith(f, childStateBlockMarker); with != 0 {
		t.Fatalf("%d child requests carried a block though the session's state context is disabled", with)
	}
}
