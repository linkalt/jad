package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadCursorIndexPersistsAndExtendsCache(t *testing.T) {
	dir := t.TempDir()
	messagesPath := filepath.Join(dir, "messages.jsonl")
	cachePath := filepath.Join(dir, "staging", "cursor-cache.json")
	journal, err := NewJournal(messagesPath, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:               1024,
		CursorCachePath:            cachePath,
		CursorCacheCheckpointBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"type":"response.completed","session_turn":1,"session_step":1}`)
	second := []byte(`{"type":"response.completed","session_turn":1,"session_step":2}`)
	third := []byte(`{"type":"response.completed","session_turn":2,"session_step":1}`)
	if err := journal.Append(first, second); err != nil {
		t.Fatal(err)
	}
	firstIndex, err := store.ReadCursorIndex()
	if err != nil {
		t.Fatalf("read initial cursor index: %v", err)
	}
	wantFirst := []CursorRun{{Offset: 0, Turn: 1, Step: 1}, {Offset: int64(len(first) + 1), Turn: 1, Step: 2}}
	if !reflect.DeepEqual(firstIndex.Runs, wantFirst) || firstIndex.Size != int64(len(first)+len(second)+2) {
		t.Fatalf("initial cursor index = %+v, want size %d and runs %+v", firstIndex, len(first)+len(second)+2, wantFirst)
	}
	contents, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read persisted cursor cache: %v", err)
	}
	var cache cursorCacheFile
	if err := json.Unmarshal(contents, &cache); err != nil {
		t.Fatalf("decode persisted cursor cache: %v", err)
	}
	if cache.SchemaVersion != cursorCacheSchemaVersion || len(cache.Runs) != 2 {
		t.Fatalf("persisted cursor cache = %+v", cache)
	}

	if err := journal.Append(third); err != nil {
		t.Fatal(err)
	}
	appended, err := store.ReadCursorIndex()
	if err != nil {
		t.Fatalf("read appended cursor index: %v", err)
	}
	wantAppended := append(append([]CursorRun(nil), wantFirst...), CursorRun{Offset: firstIndex.Size, Turn: 2, Step: 1})
	if !reflect.DeepEqual(appended.Runs, wantAppended) || appended.Size != firstIndex.Size+int64(len(third)+1) {
		t.Fatalf("appended cursor index = %+v, want size %d and runs %+v", appended, firstIndex.Size+int64(len(third)+1), wantAppended)
	}
	contents, err = os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read updated cursor cache: %v", err)
	}
	if err := json.Unmarshal(contents, &cache); err != nil || len(cache.Runs) != 3 {
		t.Fatalf("updated cursor cache = %+v, decode error %v", cache, err)
	}
}

func TestReadCursorIndexRebuildsAfterTranscriptReplacement(t *testing.T) {
	dir := t.TempDir()
	messagesPath := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(messagesPath, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"type":"response.completed","session_turn":1,"session_step":1}`)
	second := []byte(`{"type":"response.completed","session_turn":2,"session_step":1}`)
	if err := journal.Append(first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCursorIndex(); err != nil {
		t.Fatalf("read initial index: %v", err)
	}
	replacement := []byte(`{"type":"response.completed","session_turn":9,"session_step":3}`)
	if err := journal.ReplaceRecords(replacement); err != nil {
		t.Fatal(err)
	}
	index, err := store.ReadCursorIndex()
	if err != nil {
		t.Fatalf("read rebuilt index: %v", err)
	}
	want := []CursorRun{{Offset: 0, Turn: 9, Step: 3}}
	if !reflect.DeepEqual(index.Runs, want) {
		t.Fatalf("rebuilt index = %+v, want %+v", index.Runs, want)
	}
}

func TestReadCursorIndexLeavesIncompleteTailUntouched(t *testing.T) {
	dir := t.TempDir()
	messagesPath := filepath.Join(dir, "messages.jsonl")
	journal, err := NewJournal(messagesPath, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	record := []byte(`{"type":"response.completed","session_turn":1,"session_step":1}`)
	if err := journal.Append(record); err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"type":"response.completed"`)
	file, err := os.OpenFile(messagesPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(partial); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.ReadCursorIndex()
	if err != nil {
		t.Fatalf("read index with partial tail: %v", err)
	}
	after, err := os.Stat(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("cursor indexing changed transcript size from %d to %d", before.Size(), after.Size())
	}
	if len(index.Runs) != 1 || index.Runs[0].Offset != 0 {
		t.Fatalf("index with partial tail = %+v", index)
	}
}
