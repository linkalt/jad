package transcript

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// failAfter returns an append write that stores only the first limit bytes of a batch and
// then fails as a full disk would.
func failAfter(limit int) func(*os.File, []byte) error {
	return func(file *os.File, data []byte) error {
		if len(data) > limit {
			if err := writeAll(file, data[:limit]); err != nil {
				return err
			}
			return syscall.ENOSPC
		}
		return writeAll(file, data)
	}
}

// A write that fails part way through a batch used to leave the records written before it
// complete in messages.jsonl; readers then counted that prefix as a turn of its own and a
// retry duplicated it (audit B3). The file must be back to its size before the batch.
func TestAppendInputItemsFailingMidBatchLeavesTheTranscriptUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`)
	if err := store.AppendInputItems(first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	turnBefore, err := store.NextSessionTurn()
	if err != nil {
		t.Fatal(err)
	}

	itemA := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]}`)
	itemB := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"b"}]}`)
	// Room for the whole first record of the batch and a few bytes of the second.
	journal.appendWriteHook = failAfter(len(itemA) + 120)
	if _, err := store.AppendInputItemsAt(itemA, itemB); err == nil {
		t.Fatal("the append reported success although the write failed")
	}
	journal.appendWriteHook = nil

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a failed batch left bytes behind:\nbefore %q\nafter  %q", before, after)
	}
	if turnAfter, err := store.NextSessionTurn(); err != nil || turnAfter != turnBefore {
		t.Fatalf("next session turn = %d (%v), want %d: a failed batch counted as a turn", turnAfter, err, turnBefore)
	}
	if err := store.AppendInputItems(itemA, itemB); err != nil {
		t.Fatalf("append after the failed one: %v", err)
	}
	if turnAfter, err := store.NextSessionTurn(); err != nil || turnAfter != turnBefore+1 {
		t.Fatalf("next session turn = %d (%v), want %d", turnAfter, err, turnBefore+1)
	}
}
