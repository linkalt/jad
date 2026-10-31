package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingJournalStaysAbsentWhenRepairedOrRead(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, filepath.Join(root, "session"), DurabilityProcess, ResponseMetadataMinimal)
	// The directory exists and only the journal is missing, so a create would succeed.
	if err := os.MkdirAll(filepath.Dir(journal.path), 0o755); err != nil {
		t.Fatal(err)
	}
	path := journal.path
	if _, err := journal.RepairTail(); err != nil {
		t.Fatalf("RepairTail: %v", err)
	}
	if err := journal.ReadRecordsRange(0, 1<<20, func(int64, []byte) error { return nil }); err != nil {
		t.Fatalf("ReadRecordsRange: %v", err)
	}
	if _, err := store.NewInputHistory().Read(); err != nil {
		t.Fatalf("input history read: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a read created the missing journal: stat err = %v", err)
	}
}
