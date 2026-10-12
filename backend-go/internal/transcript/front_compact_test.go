package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontCompactRecoverableArchivesWholeInputBatchAndInvalidatesCaches(t *testing.T) {
	store, journal, paths := newFrontCompactTestStore(t)
	prefix := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"keep prefix"}]}`)
	if err := journal.Append(prefix); err != nil {
		t.Fatal(err)
	}
	batch := [][]byte{
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"batch one"}]}`),
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"batch two"}]}`),
	}
	if err := store.AppendInputItems(batch...); err != nil {
		t.Fatal(err)
	}
	response := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"batch answer"}]}]}}`)
	if err := journal.Append(response); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(paths.messages)
	if err != nil {
		t.Fatal(err)
	}
	var targetOffset int64 = -1
	var batchStart int64 = -1
	if err := journal.ReadRecords(func(offset int64, line []byte) error {
		if strings.Contains(string(line), "batch one") {
			batchStart = offset
		}
		if strings.Contains(string(line), "batch two") {
			targetOffset = offset
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if batchStart < 0 || targetOffset < 0 || batchStart >= targetOffset {
		t.Fatalf("could not find batched message offsets: first=%d second=%d", batchStart, targetOffset)
	}
	if err := os.MkdirAll(filepath.Dir(paths.compactionCache), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.compactionCache, paths.usageCache, paths.cursorCache, paths.stateContext} {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := store.FrontCompactFromMessage(fmt.Sprintf("session-user-o%d", targetOffset), "recoverable")
	if err != nil {
		t.Fatal(err)
	}
	if result.DeleteMode != "recoverable" || result.ArchiveName == "" {
		t.Fatalf("front-compaction result = %+v", result)
	}
	current, err := os.ReadFile(paths.messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original[:batchStart]) {
		t.Fatalf("active transcript = %q, want prefix %q", current, original[:batchStart])
	}
	archivePath := paths.messages + ".deleted-history/" + result.ArchiveName
	archived, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != string(original[batchStart:]) {
		t.Fatalf("archived transcript tail differs\n got: %q\nwant: %q", archived, original[batchStart:])
	}
	for _, path := range []string{paths.usageCache, paths.cursorCache} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("front compaction left derived cache %s: %v", path, err)
		}
	}
	if kept, err := os.ReadFile(paths.stateContext); err != nil || string(kept) != "stale" {
		t.Errorf("front compaction changed the state context snapshot: %q, err=%v", kept, err)
	}
	if _, err := os.Stat(paths.compactionCache); err != nil {
		t.Errorf("front compaction removed the compaction overlay: %v", err)
	}
	history, err := store.ReadInputItems()
	if err != nil || len(history) != 1 || !strings.Contains(string(history[0]), "keep prefix") {
		t.Fatalf("history after front compaction = %q, err=%v", history, err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.messages + ".deleted-history"); !os.IsNotExist(err) {
		t.Fatalf("reset left deleted-history archive directory: %v", err)
	}
}

func TestFrontCompactPermanentDoesNotCreateArchiveAndRejectsInvalidSelection(t *testing.T) {
	store, journal, paths := newFrontCompactTestStore(t)
	prefix := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"keep prefix"}]}`)
	target := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"remove from here"}]}`)
	assistant := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"remove"}]}]}}`)
	if err := journal.Append(prefix, target, assistant); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(paths.messages)
	if err != nil {
		t.Fatal(err)
	}
	var targetOffset int64 = -1
	if err := journal.ReadRecords(func(offset int64, line []byte) error {
		if strings.Contains(string(line), "remove from here") {
			targetOffset = offset
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if targetOffset <= 0 {
		t.Fatalf("target user message offset = %d", targetOffset)
	}
	result, err := store.FrontCompactFromMessage(fmt.Sprintf("session-user-o%d", targetOffset), "permanent")
	if err != nil {
		t.Fatal(err)
	}
	if result.DeleteMode != "permanent" || result.ArchiveName != "" {
		t.Fatalf("front-compaction result = %+v", result)
	}
	current, err := os.ReadFile(paths.messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original[:targetOffset]) {
		t.Fatalf("active transcript = %q, want prefix before target", current)
	}
	if _, err := os.Stat(paths.messages + ".deleted-history"); !os.IsNotExist(err) {
		t.Fatalf("permanent deletion created a recoverable archive directory: %v", err)
	}
	for _, invalid := range []string{"session-user-o01", "session-user-o999999"} {
		if _, err := store.FrontCompactFromMessage(invalid, "permanent"); err == nil {
			t.Errorf("FrontCompactFromMessage(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestFrontCompactRejectsActiveResponseTurn(t *testing.T) {
	store, _, _ := newFrontCompactTestStore(t)
	turn, err := store.BeginTurn("front-compact-active")
	if err != nil {
		t.Fatal(err)
	}
	defer turn.Close()
	if _, err := store.FrontCompactFromMessage("session-user-o0", "permanent"); err == nil {
		t.Fatal("front compaction succeeded with an active response turn")
	}
}

type frontCompactTestPaths struct {
	messages        string
	compactionCache string
	usageCache      string
	cursorCache     string
	stateContext    string
}

func newFrontCompactTestStore(t *testing.T) (*TurnStore, *Journal, frontCompactTestPaths) {
	t.Helper()
	dir := t.TempDir()
	paths := frontCompactTestPaths{
		messages:        filepath.Join(dir, "messages.jsonl"),
		compactionCache: filepath.Join(dir, "staging", "compaction-cache.json"),
		usageCache:      filepath.Join(dir, "staging", "usage-cache.json"),
		cursorCache:     filepath.Join(dir, "staging", "cursor-cache.json"),
		stateContext:    filepath.Join(dir, "staging", "statecontext.json"),
	}
	journal, err := NewJournal(paths.messages, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: paths.compactionCache,
		UsageCachePath:      paths.usageCache,
		CursorCachePath:     paths.cursorCache,
		StateContextPath:    paths.stateContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, journal, paths
}
