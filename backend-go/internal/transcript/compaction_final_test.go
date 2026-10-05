package transcript

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactHistoryDropsAssistantMessageSupersededByLaterFunctionCall(t *testing.T) {
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
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"superseded answer"}]}]}}`,
		`{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"tool","arguments":"{}"}]}}`,
	} {
		if err := journal.Append([]byte(response)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "replace"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || result.RetainedItems != 1 {
		t.Fatalf("compaction result = %+v", result)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !strings.Contains(string(history[0]), `"role":"user"`) {
		t.Fatalf("request history retained non-final assistant content: %q", history)
	}
}
