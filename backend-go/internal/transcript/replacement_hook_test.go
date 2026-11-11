package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The hook of ReplaceRecordsWith sees the size and digest of exactly the bytes that are published,
// and the size of the file they replace.
func TestReplaceRecordsWithReportsTheReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.ReplaceRecords([]byte(`{"a": 1}`), []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path)
	var seen Replacement
	err = journal.ReplaceRecordsWith(func(replacement Replacement) error {
		seen = replacement
		if current, _ := os.ReadFile(path); string(current) != string(old) {
			t.Error("the transcript was already replaced when the hook ran")
		}
		return nil
	}, []byte(`{"c": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	published, _ := os.ReadFile(path)
	if string(published) != "{\"c\":3}\n" {
		t.Fatalf("published %q", published)
	}
	want := Replacement{PreviousSize: int64(len(old)), Size: int64(len(published)), SHA256: sha256Hex(published)}
	if seen != want {
		t.Fatalf("hook saw %+v, want %+v", seen, want)
	}
}

// An error from the hook means the replacement did not happen, and leaves no temporary file.
func TestReplaceRecordsWithHookErrorAbandonsTheReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.ReplaceRecords([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	if err := journal.ReplaceRecordsWith(func(Replacement) error { return refused }, []byte(`{"b":2}`)); !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the hook's", err)
	}
	if current, _ := os.ReadFile(path); string(current) != "{\"a\":1}\n" {
		t.Fatalf("transcript = %q after a refused replacement", current)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries after a refused replacement, want only the transcript", len(entries))
	}
}

func TestReplaceWithPrefixWithReportsThePrefixAndRefusesBeforeArchiving(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.ReplaceRecords([]byte(`{"a":1}`), []byte(`{"b":2}`), []byte(`{"c":3}`)); err != nil {
		t.Fatal(err)
	}
	whole, _ := os.ReadFile(path)
	boundary := int64(len("{\"a\":1}\n{\"b\":2}\n"))
	archive := filepath.Join(dir, "archive", "removed.jsonl")

	refused := errors.New("refused")
	var seen Replacement
	err = journal.ReplaceWithPrefixWith(func(replacement Replacement) error {
		seen = replacement
		return refused
	}, boundary, int64(len(whole)), archive)
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the hook's", err)
	}
	if current, _ := os.ReadFile(path); string(current) != string(whole) {
		t.Fatal("a refused front compaction changed the transcript")
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused front compaction wrote its archive: %v", err)
	}
	want := Replacement{PreviousSize: int64(len(whole)), Size: boundary, SHA256: sha256Hex(whole[:boundary])}
	if seen != want {
		t.Fatalf("hook saw %+v, want %+v", seen, want)
	}

	if err := journal.ReplaceWithPrefixWith(func(Replacement) error { return nil }, boundary, int64(len(whole)), archive); err != nil {
		t.Fatal(err)
	}
	if current, _ := os.ReadFile(path); string(current) != string(whole[:boundary]) {
		t.Fatalf("transcript = %q", current)
	}
}

// A compaction asks its hook twice with the same replacement: once before it writes anything and
// once right before the rename, where the file it reports is the one that is published.
func TestCompactHistoryAsksBeforePublishTwiceWithTheSameReplacement(t *testing.T) {
	store, messages, _ := compactionStore(t)
	appendTurns(t, store, 1, 3)
	before, _ := os.ReadFile(messages)
	var asked []Replacement
	result, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace", BeforePublish: func(replacement Replacement) error {
		asked = append(asked, replacement)
		return nil
	}})
	if err != nil || !result.Compacted {
		t.Fatalf("compaction: %+v, %v", result, err)
	}
	published, _ := os.ReadFile(messages)
	want := Replacement{PreviousSize: int64(len(before)), Size: int64(len(published)), SHA256: sha256Hex(published)}
	if len(asked) != 2 || asked[0] != want || asked[1] != want {
		t.Fatalf("hook was asked %+v, want twice %+v", asked, want)
	}
}

// A hook that refuses before anything is written leaves the transcript, the backup and the
// compaction cache as they were; one that refuses at the rename leaves the transcript.
func TestCompactHistoryBeforePublishErrorLeavesTheTranscript(t *testing.T) {
	for _, refuseAt := range []int{1, 2} {
		store, messages, backup := compactionStore(t)
		appendTurns(t, store, 1, 3)
		before, _ := os.ReadFile(messages)
		refused := errors.New("refused")
		calls := 0
		_, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace", BeforePublish: func(Replacement) error {
			calls++
			if calls == refuseAt {
				return refused
			}
			return nil
		}})
		if !errors.Is(err, refused) || calls != refuseAt {
			t.Fatalf("refuse at %d: error %v after %d calls", refuseAt, err, calls)
		}
		if after, _ := os.ReadFile(messages); string(after) != string(before) {
			t.Fatalf("refuse at %d: transcript changed although the hook refused", refuseAt)
		}
		if _, err := os.Stat(backup); refuseAt == 1 && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a compaction refused before it wrote anything wrote its backup (%v)", err)
		}
	}
}

// Only a reset deletes the usage baseline; a compaction deletes the usage cache and leaves it.
func TestOnlyResetRemovesTheUsageBaseline(t *testing.T) {
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	cache := filepath.Join(dir, "staging", "usage-cache.json")
	baseline := cache + ".baseline"
	journal, err := NewJournal(messages, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
		UsageCachePath:      cache,
		UsageBaselinePath:   baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	appendTurns(t, store, 1, 3)
	for _, path := range []string{cache, baseline} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := store.CompactHistory(CompactionOptions{TurnType: "full", KeepTurns: 1, BackupMode: "replace"}); err != nil || !result.Compacted {
		t.Fatalf("compaction: %+v, %v", result, err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("compaction kept the usage cache (%v)", err)
	}
	if _, err := os.Stat(baseline); err != nil {
		t.Fatalf("compaction removed the usage baseline: %v", err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(baseline); !os.IsNotExist(err) {
		t.Fatalf("reset kept the usage baseline (%v)", err)
	}
}
