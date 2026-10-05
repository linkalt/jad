package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateContextSnapshotReadWriteIsSeparateFromTranscript(t *testing.T) {
	root := t.TempDir()
	messagesPath := filepath.Join(root, "messages.jsonl")
	journal, err := NewJournal(messagesPath, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "staging", "statecontext.json")
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		MaxItemBytes:     1024,
		StateContextPath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := StateContextSnapshot{
		SchemaVersion: 1,
		GeneratedAt:   "2026-10-03T07:17:47.793Z",
		Trigger:       "compaction",
		Text:          "STATE CONTEXT SNAPSHOT\n{}",
	}
	if err := store.WriteStateContextSnapshot(want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := store.ReadStateContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || got != want {
		t.Fatalf("ReadStateContextSnapshot() = %#v, %v; want %#v, true", got, exists, want)
	}
	if has, err := store.HasStateContextSnapshot(); err != nil || !has {
		t.Fatalf("HasStateContextSnapshot() = %v, %v; want true, nil", has, err)
	}
	if _, err := os.Stat(messagesPath); !os.IsNotExist(err) {
		t.Fatalf("writing state context changed messages.jsonl, stat error = %v", err)
	}

	if err := os.WriteFile(statePath, []byte(`{"schema_version":2,"generated_at":"x","trigger":"compaction","text":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.ReadStateContextSnapshot(); err != nil || exists {
		t.Fatalf("malformed snapshot read = exists %v, err %v; want false, nil", exists, err)
	}
}
