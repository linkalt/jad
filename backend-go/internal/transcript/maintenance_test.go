package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactHistoryPersistsCompactionOverlayAndResetInvalidatesFiles(t *testing.T) {
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	backup := filepath.Join(dir, "messages.backup.jsonl")
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(messages, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  backup,
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"old context that should be removed", "latest context to keep"} {
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`)); err != nil {
			t.Fatal(err)
		}
		response := []byte(`{"type":"response.completed","response":{"id":"resp_` + string(rune('1'+i)) + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer ` + string(rune('1'+i)) + `"}]}]}}`)
		if err := journal.Append(response); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || result.CompactedItems != 4 || result.RetainedItems != 2 {
		t.Fatalf("compaction result = %+v", result)
	}
	transcript, err := os.ReadFile(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 0 {
		t.Fatalf("messages.jsonl after compaction = %q, want empty transcript plus cache overlay", transcript)
	}
	archived, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(archived), "response.completed") != 2 {
		t.Fatalf("backup transcript does not contain both responses: %s", archived)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !strings.Contains(string(history[0]), "latest context") || !strings.Contains(string(history[1]), "answer 2") {
		t.Fatalf("compacted request history = %q", history)
	}
	if err := os.WriteFile(filepath.Join(staging, "usage-cache.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{backup, filepath.Join(staging, "compaction-cache.json"), filepath.Join(staging, "compaction-cache.json.backup"), filepath.Join(staging, "usage-cache.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset left %s: %v", path, err)
		}
	}
	history, err = store.ReadInputItems()
	if err != nil || len(history) != 0 {
		t.Fatalf("history after reset = %q, err=%v", history, err)
	}
}

func mustReadInputItems(t *testing.T, store *TurnStore) [][]byte {
	t.Helper()
	items, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestResetKeepsTheTranscriptWhenTheCompactionCacheCannotBeRemoved(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 2)
	f.addTurn(t, 4)
	before := mustReadInputItems(t, f.store)
	transcript, err := os.ReadFile(f.journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) == 0 {
		t.Fatal("precondition: transcript is empty, so a publish would not show")
	}
	staging := filepath.Join(f.dir, "staging")
	if err := os.Chmod(staging, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(staging, 0o700) })
	if err := f.store.Reset(); err == nil {
		t.Skip("the staging directory is writable despite mode 0500, as for root")
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(f.journal.path); err != nil || string(got) != string(transcript) {
		t.Fatalf("transcript after failed reset = %q, err=%v; want unchanged %q", got, err, transcript)
	}
	if after := mustReadInputItems(t, f.store); len(after) != len(before) {
		t.Fatalf("history after failed reset has %d items, want %d unchanged", len(after), len(before))
	}
}

func TestCompactHistoryRemovesTheCacheWhenTheTranscriptReplaceFails(t *testing.T) {
	dir := t.TempDir()
	transcriptDir := filepath.Join(dir, "transcript")
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(filepath.Join(transcriptDir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		ResponseMetadata:    ResponseMetadataMinimal,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := memoFixture{dir: dir, journal: journal, store: store}
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	before := mustReadInputItems(t, store)
	if err := os.Chmod(transcriptDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(transcriptDir, 0o700) })
	_, compactErr := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 2, BackupMode: "append"})
	if err := os.Chmod(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if compactErr == nil {
		t.Skip("the transcript directory is writable despite mode 0500, as for root")
	}
	if _, err := os.Stat(f.compactionCachePath()); !os.IsNotExist(err) {
		t.Fatalf("compaction cache after failed replace: err=%v, want removed", err)
	}
	if after := mustReadInputItems(t, store); len(after) != len(before) {
		t.Fatalf("history after failed compaction has %d items, want %d unchanged", len(after), len(before))
	}
}

func TestCompactHistoryNoOpWhenRecentTurnDoesNotReduceHistory(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20, MessagesBackupPath: filepath.Join(dir, "backup.jsonl"), CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"same"}]}`)); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "replace"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Compacted || result.CompactedItems != 0 || result.RetainedItems != 0 || result.BackupPath != "" {
		t.Fatalf("unexpected no-op compaction result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("no-op compaction created backup: %v", err)
	}
}
