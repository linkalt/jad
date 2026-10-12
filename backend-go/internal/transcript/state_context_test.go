package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStateContextStore(t *testing.T) (*TurnStore, string, string) {
	t.Helper()
	root := t.TempDir()
	messagesPath := filepath.Join(root, "messages.jsonl")
	journal, err := NewJournal(messagesPath, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "staging", "statecontext.jsonl")
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(root, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(root, "staging", "compaction-cache.json"),
		StateContextPath:    statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, messagesPath, statePath
}

func stateSnapshot(trigger, text string) StateContextSnapshot {
	return StateContextSnapshot{SchemaVersion: 1, GeneratedAt: "2026-10-03T07:17:47.793Z", Trigger: trigger, Text: text}
}

func appendUserMessages(t *testing.T, store *TurnStore, texts ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(texts))
	for _, text := range texts {
		result, err := store.AppendInputItemsAt([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fmt.Sprintf("session-user-o%d", result.MessageOffset))
	}
	return ids
}

func TestStateContextEntriesAreAppendedAfterTheCurrentHistory(t *testing.T) {
	store, messagesPath, _ := newStateContextStore(t)
	before, _ := os.ReadFile(messagesPath)
	first, err := store.AppendStateContext(stateSnapshot("session_start", "first"), "developer")
	if err != nil {
		t.Fatal(err)
	}
	if first.After != 0 || first.Role != "developer" {
		t.Fatalf("first entry = %#v, want after 0", first)
	}
	if after, _ := os.ReadFile(messagesPath); string(after) != string(before) {
		t.Fatalf("writing state context changed messages.jsonl")
	}
	appendUserMessages(t, store, "one", "two")
	second, err := store.AppendStateContext(stateSnapshot("compaction", "second"), "system")
	if err != nil {
		t.Fatal(err)
	}
	if second.After != 2 {
		t.Fatalf("second entry after = %d, want 2", second.After)
	}
	entries, err := store.ReadStateContextEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0] != first || entries[1] != second {
		t.Fatalf("entries = %#v, want the two appended in order", entries)
	}
}

func TestStateContextEntriesSkipMalformedLinesAndMissingFile(t *testing.T) {
	store, _, statePath := newStateContextStore(t)
	if entries, err := store.ReadStateContextEntries(); err != nil || len(entries) != 0 {
		t.Fatalf("missing file = %#v, %v; want none", entries, err)
	}
	if _, err := store.AppendStateContext(stateSnapshot("compaction", "good"), "developer"); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		`not json`,
		`{"schema_version":2,"generated_at":"x","trigger":"compaction","text":"x","role":"developer","after":0}`,
		`{"schema_version":1,"generated_at":"x","trigger":"compaction","text":"x","role":"","after":0}`,
		`{"schema_version":1,"generated_at":"x","trigger":"compaction","text":"x","role":"developer","after":-1}`,
		`{"schema_version":1,"generated_at":"x","trigger":"compaction","role":"developer","after":0}`,
	}
	file, err := os.OpenFile(statePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Join(bad, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	entries, err := store.ReadStateContextEntries()
	if err != nil || len(entries) != 1 || entries[0].Text != "good" {
		t.Fatalf("entries = %#v, %v; want only the valid line", entries, err)
	}
}

func TestAppendStateContextRejectsBadInput(t *testing.T) {
	store, _, _ := newStateContextStore(t)
	bad := stateSnapshot("compaction", "x")
	bad.SchemaVersion = 2
	if _, err := store.AppendStateContext(bad, "developer"); err == nil {
		t.Fatal("schema version 2 was accepted")
	}
	if _, err := store.AppendStateContext(stateSnapshot("compaction", "x"), ""); err == nil {
		t.Fatal("empty role was accepted")
	}
}

func TestFrontCompactKeepsOnlyTheLatestStrandedStateContext(t *testing.T) {
	store, _, statePath := newStateContextStore(t)
	appendUserMessages(t, store, "one")
	if _, err := store.AppendStateContext(stateSnapshot("session_start", "early"), "developer"); err != nil {
		t.Fatal(err)
	}
	ids := appendUserMessages(t, store, "two", "three")
	if _, err := store.AppendStateContext(stateSnapshot("compaction", "stranded-old"), "developer"); err != nil {
		t.Fatal(err)
	}
	appendUserMessages(t, store, "four")
	if _, err := store.AppendStateContext(stateSnapshot("compaction", "stranded-new"), "developer"); err != nil {
		t.Fatal(err)
	}
	// Remove everything from the second message on: items 1..4 are gone.
	if _, err := store.FrontCompactFromMessage(ids[0], "permanent"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ReadStateContextEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Text != "early" || entries[0].After != 1 ||
		entries[1].Text != "stranded-new" || entries[1].After != 1 {
		t.Fatalf("entries = %#v, want early (after 1) and only the latest stranded block re-anchored to 1", entries)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal(err)
	}
}

func TestResetRemovesEveryStateContextEntry(t *testing.T) {
	store, _, statePath := newStateContextStore(t)
	if _, err := store.AppendStateContext(stateSnapshot("session_start", "x"), "developer"); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("reset left the state context file, stat error = %v", err)
	}
}

func TestCompactionRemovesEveryStateContextEntry(t *testing.T) {
	store, _, statePath := newStateContextStore(t)
	appendUserMessages(t, store, "old", "newer")
	if _, err := store.AppendStateContext(stateSnapshot("session_start", "x"), "developer"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"resp_1", "resp_2"} {
		if err := store.journal.Append([]byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}}`)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"})
	if err != nil || !result.Compacted {
		t.Fatalf("compaction = %+v, %v; want compacted", result, err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("compaction left the state context file, stat error = %v", err)
	}
}
