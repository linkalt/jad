package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCacheSourceIsTranscript(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20, ResponseMetadata: ResponseMetadataMinimal})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"aaaa"}]}`)); err != nil {
		t.Fatal(err)
	}
	hash, size, err := hashTranscriptFile(journal.path)
	if err != nil || size == 0 {
		t.Fatalf("hashTranscriptFile() = %q, %d, %v", hash, size, err)
	}
	check := func(name string, cache compactionCache, want bool) {
		t.Helper()
		got, err := store.cacheSourceIsTranscript(&cache)
		if err != nil || got != want {
			t.Errorf("%s: cacheSourceIsTranscript() = %v, %v, want %v", name, got, err, want)
		}
	}
	withSize := compactionCache{SourceSHA256: hash, SourceBytes: size}
	withoutSize := compactionCache{SourceSHA256: hash}
	check("unchanged transcript, size recorded", withSize, true)
	check("unchanged transcript, size not recorded", withoutSize, true)

	original, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]byte(nil), original...)
	edited[len(edited)-10] ^= 1
	if err := os.WriteFile(journal.path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	check("same size, one byte changed, size recorded", withSize, false)
	check("same size, one byte changed, size not recorded", withoutSize, false)

	if err := os.WriteFile(journal.path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(`{"type":"function_call_output","call_id":"c","output":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	check("transcript grew, size recorded", withSize, false)
	check("transcript grew, size not recorded", withoutSize, false)

	if err := os.Remove(journal.path); err != nil {
		t.Fatal(err)
	}
	check("transcript missing, size recorded", withSize, false)
}

// A compaction cache written before source_bytes existed has no size and must read
// the same history as one that has it, before and after the transcript grows.
func TestReadInputItemsWithCacheWithoutSourceBytes(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		ResponseMetadata:    ResponseMetadataMinimal,
		MessagesBackupPath:  filepath.Join(dir, "backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var records [][]byte
	for turn := 0; turn < 20; turn++ {
		records = append(records,
			[]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d %s"}]}`, turn, strings.Repeat("q", 200))),
			[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d %s"}]}]}}`, turn, turn, strings.Repeat("a", 200))))
	}
	if err := journal.Append(records...); err != nil {
		t.Fatal(err)
	}
	if result, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 5, BackupMode: "replace"}); err != nil || !result.Compacted {
		t.Fatalf("CompactHistory() = %+v, %v", result, err)
	}
	encoded, err := os.ReadFile(store.options.CompactionCachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cache map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &cache); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache["source_bytes"]; !ok {
		t.Fatalf("compaction cache has no source_bytes: %s", encoded[:200])
	}
	want, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	delete(cache, "source_bytes")
	legacy, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.options.CompactionCachePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"after compaction", "after growth"} {
		got, err := store.ReadInputItems()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: legacy cache read %d items, size-aware cache read %d", step, len(got), len(want))
		}
		extra := []byte(`{"type":"function_call_output","call_id":"c","output":"ok"}`)
		if err := journal.Append(extra); err != nil {
			t.Fatal(err)
		}
		want = append(want, extra)
	}
}
