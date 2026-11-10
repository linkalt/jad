package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeResetFiles puts a stale file at every path a reset removes, and returns them.
func writeResetFiles(t *testing.T, f memoFixture) []string {
	t.Helper()
	staging := filepath.Join(f.dir, "staging")
	paths := []string{
		filepath.Join(f.dir, "messages.backup.jsonl"),
		filepath.Join(staging, "compaction-cache.json"),
		filepath.Join(staging, "compaction-cache.json.backup"),
		filepath.Join(staging, "cursor-cache.json"),
		filepath.Join(staging, "statecontext.json"),
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func requireGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset left %s: %v", path, err)
		}
	}
}

// A reset that fails while removing a derived file leaves the history in place, because the
// empty transcript is published last. The same call, once the cause is gone, finishes the job:
// a transcript that was emptied with caches left behind is what a caller that skips empty
// transcripts would never retry.
func TestResetFailingOnADerivedFileKeepsTheHistoryAndRetries(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	f.addTurn(t, 1)
	before, err := os.ReadFile(f.journal.path)
	if err != nil || len(before) == 0 {
		t.Fatalf("precondition: transcript %q, %v", before, err)
	}
	items := mustReadInputItems(t, f.store)
	paths := writeResetFiles(t, f)
	usage := filepath.Join(f.dir, "staging", "usage-cache.json")
	if err := os.MkdirAll(filepath.Join(usage, "obstacle"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := f.journal.path + ".deleted-history"
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Reset(); err == nil {
		t.Fatal("reset succeeded although the usage cache could not be removed")
	}
	if got, err := os.ReadFile(f.journal.path); err != nil || string(got) != string(before) {
		t.Fatalf("transcript after the failed reset = %q, %v; want it unchanged", got, err)
	}
	if after := mustReadInputItems(t, f.store); len(after) != len(items) {
		t.Fatalf("history after the failed reset has %d items, want %d", len(after), len(items))
	}
	if err := os.RemoveAll(usage); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Reset(); err != nil {
		t.Fatalf("reset after the cause was removed: %v", err)
	}
	if got, err := os.ReadFile(f.journal.path); err != nil || len(got) != 0 {
		t.Fatalf("transcript after the retry = %q, %v; want empty", got, err)
	}
	requireGone(t, append(paths, usage, archive)...)
	// A reset of what is already reset is fine too (a retry after a crash can meet this).
	if err := f.store.Reset(); err != nil {
		t.Fatalf("reset of an already reset history: %v", err)
	}
}

// A reset whose publish fails (the transcript directory cannot be written) leaves the history
// in place; what was removed before it is what the reset deletes anyway, and the next reset
// finishes.
func TestResetFailingToPublishKeepsTheHistoryAndRetries(t *testing.T) {
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
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := memoFixture{dir: dir, journal: journal, store: store}
	f.addTurn(t, 0)
	before, _ := os.ReadFile(journal.path)
	usage := filepath.Join(staging, "usage-cache.json")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usage, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transcriptDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(transcriptDir, 0o700) })
	if err := store.Reset(); err == nil {
		t.Skip("the transcript directory is writable despite mode 0500, as for root")
	}
	if err := os.Chmod(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(journal.path); err != nil || string(got) != string(before) || !strings.Contains(string(got), "question 0") {
		t.Fatalf("transcript after the failed publish = %q, %v; want it unchanged", got, err)
	}
	if err := store.Reset(); err != nil {
		t.Fatalf("reset after the directory was writable again: %v", err)
	}
	if got, err := os.ReadFile(journal.path); err != nil || len(got) != 0 {
		t.Fatalf("transcript after the retry = %q, %v; want empty", got, err)
	}
	requireGone(t, usage)
}

// strictResetFixture is a store whose journal syncs the directory after a replace, with the
// sync replaceable so a test can make it fail after the rename succeeded.
func strictResetFixture(t *testing.T) (*TurnStore, *Journal, string) {
	t.Helper()
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := memoFixture{dir: dir, journal: journal, store: store}
	f.addTurn(t, 0)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	return store, journal, staging
}

// The rename of the empty transcript succeeding while the directory sync fails is a reset that
// happened: Reset reports it as a *PublishedError (so a caller stops the children and does not
// believe nothing changed), the history is empty, every derived file is gone, and the error
// says the outcome is unconfirmed. A replace whose rename fails is not a PublishedError.
func TestResetWhoseDirectorySyncFailsAfterTheRenameReportsItAsPublished(t *testing.T) {
	store, journal, staging := strictResetFixture(t)
	usage := filepath.Join(staging, "usage-cache.json")
	if err := os.WriteFile(usage, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	journal.dirSyncHook = func(string) error {
		syncs++
		return errors.New("disk would not sync")
	}
	err := store.Reset()
	var published *PublishedError
	if !errors.As(err, &published) || !strings.Contains(err.Error(), "not confirmed durable") || !strings.Contains(err.Error(), "disk would not sync") {
		t.Fatalf("Reset error = %v, want a *PublishedError saying the reset is applied but not confirmed durable", err)
	}
	if syncs == 0 {
		t.Fatal("the injected directory sync never ran")
	}
	if got, readErr := os.ReadFile(journal.path); readErr != nil || len(got) != 0 {
		t.Fatalf("transcript after the published reset = %q, %v; want empty", got, readErr)
	}
	requireGone(t, usage)
	if items := mustReadInputItems(t, store); len(items) != 0 {
		t.Fatalf("history after the published reset has %d items", len(items))
	}
	// With the sync working again the retry succeeds on the empty history.
	journal.dirSyncHook = nil
	if err := store.Reset(); err != nil {
		t.Fatalf("retry after the sync recovered: %v", err)
	}
}

func TestReplaceRecordsReportsOnlyAPostRenameFailureAsPublished(t *testing.T) {
	_, journal, _ := strictResetFixture(t)
	journal.dirSyncHook = func(string) error { return errors.New("sync failed") }
	var published *PublishedError
	if err := journal.ReplaceRecords(); !errors.As(err, &published) {
		t.Fatalf("a failed directory sync after the rename = %v, want *PublishedError", err)
	}
	journal.dirSyncHook = nil
	if err := journal.ReplaceRecords([]byte("not json")); err == nil || errors.As(err, &published) {
		t.Fatalf("a record that cannot be written = %v, want a plain error (nothing was replaced)", err)
	}
}
