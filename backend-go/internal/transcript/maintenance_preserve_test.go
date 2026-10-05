package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactHistoryPreservesOnlyActiveToolCalls(t *testing.T) {
	root := t.TempDir()
	messagesPath := filepath.Join(root, "messages.jsonl")
	staging := filepath.Join(root, "staging")
	journal, err := NewJournal(messagesPath, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(root, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"older context"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"resp-old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old ` + strings.Repeat("detail ", 30) + `"}]}]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"current request"}]}`)); err != nil {
		t.Fatal(err)
	}
	current := `{"type":"response.completed","response":{"id":"resp-active","status":"completed","output_text":"temporary convenience field","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"private reasoning"}]},{"type":"function_call","call_id":"call-active","name":"lookup","arguments":"{}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"temporary assistant text"}]}]}}`
	if err := journal.Append([]byte(current)); err != nil {
		t.Fatal(err)
	}

	result, err := store.CompactHistory(CompactionOptions{
		TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", PreserveResponseID: "resp-active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || result.RetainedItems != 2 {
		t.Fatalf("compaction result = %+v, want compacted with current user and tool call retained", result)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !strings.Contains(string(history[0]), "current request") || !strings.Contains(string(history[1]), "call-active") {
		t.Fatalf("history after compaction = %q", history)
	}
	if strings.Contains(string(history[1]), "temporary assistant text") || strings.Contains(string(history[0]), "older context") {
		t.Fatalf("compaction retained older or non-tool active response content: %q", history)
	}
	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !strings.Contains(string(records[0]), `"id":"resp-active"`) || strings.Contains(string(records[0]), "temporary assistant text") {
		t.Fatalf("messages.jsonl active tail = %q", records)
	}
	wantActiveTail := `{"type":"response.completed","response":{"id":"resp-active","status":"completed","output":[{"type":"function_call","call_id":"call-active","name":"lookup","arguments":"{}"}]}}`
	if string(records[0]) != wantActiveTail {
		t.Fatalf("messages.jsonl active tail = %s, want Node JSON.stringify golden %s", records[0], wantActiveTail)
	}
	if _, err := os.Stat(filepath.Join(root, "messages.backup.jsonl")); err != nil {
		t.Fatalf("compaction backup: %v", err)
	}
}

func TestCompactHistoryRejectsMissingPreservedResponse(t *testing.T) {
	root := t.TempDir()
	journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		CompactionCachePath: filepath.Join(root, "staging", "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"keep me"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err = store.CompactHistory(CompactionOptions{
		TurnType: "assistant_final", KeepTurns: 0, BackupMode: "replace", PreserveResponseID: "missing-response",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot preserve a response") {
		t.Fatalf("CompactHistory() error = %v, want missing preserved response error", err)
	}
	var event json.RawMessage
	if err := journal.ReadLines(func(line []byte) error {
		event = append(event[:0], line...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(event) == 0 {
		t.Fatal("missing preserved response failure modified the transcript")
	}
}
