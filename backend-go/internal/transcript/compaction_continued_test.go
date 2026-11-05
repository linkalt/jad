package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type steeredFixture struct {
	dir     string
	journal *Journal
	store   *TurnStore
}

// newSteeredFixture holds an older turn, then a run's first message with a tool call,
// then a message of that run stored as steering input (or as ordinary input when steer is
// false) and the answer to it.
func newSteeredFixture(t *testing.T, steer bool) steeredFixture {
	t.Helper()
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := steeredFixture{dir: dir, journal: journal, store: store}
	f.user("older context")
	f.response("resp_old", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+strings.Repeat("old detail ", 40)+`"}]}`)
	f.user("run request")
	f.response("resp_call", `{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"}`)
	if err := store.AppendInputItems([]byte(`{"type":"function_call_output","call_id":"call_1","output":"tool result"}`)); err != nil {
		t.Fatal(err)
	}
	steering := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"steering message"}]}`)
	if steer {
		_, err = store.AppendSteerInputItemsAt(steering)
	} else {
		err = store.AppendInputItems(steering)
	}
	if err != nil {
		t.Fatal(err)
	}
	f.response("resp_final", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`)
	return f
}

func (f steeredFixture) user(text string) {
	if err := f.store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`)); err != nil {
		panic(err)
	}
}

func (f steeredFixture) response(id, output string) {
	if err := f.journal.Append([]byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[` + output + `]}}`)); err != nil {
		panic(err)
	}
}

func (f steeredFixture) history(t *testing.T) string {
	t.Helper()
	history, err := f.store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	return all
}

func (f steeredFixture) compact(t *testing.T, keepTurns int) CompactionResult {
	t.Helper()
	result, err := f.store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: keepTurns, BackupMode: "replace"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A steering message is written with the steer field, which never reaches the history
// that is sent to the provider, and a client cannot send the field itself.
func TestSteerInputIsMarkedInTheTranscriptOnly(t *testing.T) {
	f := newSteeredFixture(t, true)
	data, err := os.ReadFile(filepath.Join(f.dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"`+SessionInputSteerField+`":true`) {
			marked++
			if !strings.Contains(line, "steering message") {
				t.Fatalf("the steer field is on another record: %s", line)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("records with the steer field = %d, want 1:\n%s", marked, data)
	}
	if strings.Contains(f.history(t), SessionInputSteerField) {
		t.Fatalf("the steer field reached the history: %s", f.history(t))
	}
	if err := ValidateInputItem(0, []byte(`{"type":"message","role":"user","content":"x","`+SessionInputSteerField+`":true}`)); err == nil {
		t.Fatal("an input item with the steer field was accepted")
	}
}

// A steering message joins the turn before it, so keep_turns 1 keeps the whole run's
// messages: its first message, the steering message and the answer. (The tool pair goes,
// as from any kept turn with keep_tool_pairs 0.)
func TestCompactHistoryKeepsASteeringMessageInTheRunsTurn(t *testing.T) {
	f := newSteeredFixture(t, true)
	if result := f.compact(t, 1); !result.Compacted {
		t.Fatalf("compaction result = %+v", result)
	}
	all := f.history(t)
	for _, want := range []string{"run request", "steering message", "done"} {
		if !strings.Contains(all, want) {
			t.Fatalf("history is missing %s: %s", want, all)
		}
	}
	if strings.Contains(all, "older context") {
		t.Fatalf("history kept the older turn: %s", all)
	}
}

// The mark outlives the compaction that moved the steering message into the compaction
// cache: a second compaction still keeps it with the run's first message.
func TestCompactHistoryKeepsTheSteerMarkAcrossCompactions(t *testing.T) {
	f := newSteeredFixture(t, true)
	if result := f.compact(t, 1); !result.Compacted {
		t.Fatalf("first compaction result = %+v", result)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "staging", "compaction-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cache compactionCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	if len(cache.ContinuedItems) != 1 || !strings.Contains(string(cache.Items[cache.ContinuedItems[0]]), "steering message") {
		t.Fatalf("continued_items = %v of %q", cache.ContinuedItems, cache.Items)
	}
	f.response("resp_more", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+strings.Repeat("more detail ", 40)+`"}]}`)
	f.compact(t, 1)
	if all := f.history(t); !strings.Contains(all, "run request") || !strings.Contains(all, "steering message") {
		t.Fatalf("the second compaction split the run at its steering message: %s", all)
	}
}

// Ordinary input, and a transcript or cache written before the mark existed, split at
// every user message as before: keep_turns 1 keeps only the last message and its answer.
func TestCompactHistorySplitsAtEveryUnmarkedUserMessage(t *testing.T) {
	f := newSteeredFixture(t, false)
	f.compact(t, 1)
	if all := f.history(t); strings.Contains(all, "run request") || !strings.Contains(all, "steering message") {
		t.Fatalf("history = %s", all)
	}
}
