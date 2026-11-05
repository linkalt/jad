package transcript

import (
	"path/filepath"
	"strings"
	"testing"
)

// continuedHistoryStore holds an older turn, then a run's first message with a tool call,
// then a steering message of that run and the answer to it.
func continuedHistoryStore(t *testing.T) *TurnStore {
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
	user := func(text string) {
		t.Helper()
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`)); err != nil {
			t.Fatal(err)
		}
	}
	response := func(id, output string) {
		t.Helper()
		if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[` + output + `]}}`)); err != nil {
			t.Fatal(err)
		}
	}
	user("older context")
	response("resp_old", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+strings.Repeat("old detail ", 40)+`"}]}`)
	user("run request")
	response("resp_call", `{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"}`)
	if err := store.AppendInputItems([]byte(`{"type":"function_call_output","call_id":"call_1","output":"tool result"}`)); err != nil {
		t.Fatal(err)
	}
	user("steering message")
	response("resp_final", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`)
	return store
}

func historyText(t *testing.T, store *TurnStore) string {
	t.Helper()
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	return all
}

// The last ContinuedUserMessages user messages join the turn before them, so keep_turns 1
// keeps the whole run's messages: its first message, the steering message and the answer.
// (The tool pair goes, as from any kept turn with keep_tool_pairs 0.)
func TestCompactHistoryKeepsContinuedUserMessagesInTheTurnBefore(t *testing.T) {
	store := continuedHistoryStore(t)
	result, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace", ContinuedUserMessages: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted {
		t.Fatalf("compaction result = %+v", result)
	}
	all := historyText(t, store)
	for _, want := range []string{"run request", "steering message", "done"} {
		if !strings.Contains(all, want) {
			t.Fatalf("history is missing %s: %s", want, all)
		}
	}
	if strings.Contains(all, "older context") {
		t.Fatalf("history kept the older turn: %s", all)
	}
}

// Without the count every user message starts a turn, as before: keep_turns 1 keeps only
// the steering message and its answer.
func TestCompactHistorySplitsAtEveryUserMessageByDefault(t *testing.T) {
	store := continuedHistoryStore(t)
	if _, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace"}); err != nil {
		t.Fatal(err)
	}
	all := historyText(t, store)
	if strings.Contains(all, "run request") || !strings.Contains(all, "steering message") {
		t.Fatalf("history = %s", all)
	}
}

// A count beyond the user messages there are joins them all into one turn, so keep_turns 1
// keeps every message; a negative count is refused.
func TestCompactHistoryContinuedUserMessagesBounds(t *testing.T) {
	store := continuedHistoryStore(t)
	result, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace", ContinuedUserMessages: 9})
	if err != nil {
		t.Fatal(err)
	}
	if all := historyText(t, store); !strings.Contains(all, "older context") || !strings.Contains(all, "run request") {
		t.Fatalf("compaction result = %+v, want every message kept: %s", result, all)
	}
	if _, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace", ContinuedUserMessages: -1}); err == nil {
		t.Fatal("a negative ContinuedUserMessages was accepted")
	}
}
