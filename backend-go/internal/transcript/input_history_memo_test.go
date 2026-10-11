package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memoFixture struct {
	dir     string
	journal *Journal
	store   *TurnStore
}

func newMemoFixture(t *testing.T) memoFixture {
	t.Helper()
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, staging, TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		ResponseMetadata:    ResponseMetadataMinimal,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
		UsageCachePath:      filepath.Join(staging, "usage-cache.json"),
		CursorCachePath:     filepath.Join(staging, "cursor-cache.json"),
		StateContextPath:    filepath.Join(staging, "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return memoFixture{dir: dir, journal: journal, store: store}
}

func (f memoFixture) addTurn(t *testing.T, index int) {
	t.Helper()
	if err := f.store.AppendInputItems([]byte(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"question %d"}]}`, index))); err != nil {
		t.Fatal(err)
	}
	response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer %d"}]}]}}`, index, index)
	if err := f.journal.Append([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

// assertSameAsUncached fails unless the handle returns byte-for-byte what a
// fresh full read returns.
func assertSameAsUncached(t *testing.T, store *TurnStore, history *InputHistory) [][]byte {
	t.Helper()
	got, err := history.Read()
	if err != nil {
		t.Fatalf("InputHistory.Read() error = %v", err)
	}
	want, err := store.ReadInputItems()
	if err != nil {
		t.Fatalf("ReadInputItems() error = %v", err)
	}
	if !equalJSONLines(got, want) {
		t.Fatalf("InputHistory.Read() differs from ReadInputItems():\n got  %q\n want %q", got, want)
	}
	return got
}

func TestInputHistoryMatchesFullReadAsTheTranscriptGrows(t *testing.T) {
	f := newMemoFixture(t)
	history := f.store.NewInputHistory()
	if items := assertSameAsUncached(t, f.store, history); len(items) != 0 {
		t.Fatalf("empty transcript returned %d items", len(items))
	}
	for index := 0; index < 6; index++ {
		f.addTurn(t, index)
		items := assertSameAsUncached(t, f.store, history)
		if len(items) != 2*(index+1) {
			t.Fatalf("after turn %d: %d items, want %d", index, len(items), 2*(index+1))
		}
	}
	// A read with nothing new is stable.
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryExpandsOnlyAppendedRecords(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	history := f.store.NewInputHistory()
	first := assertSameAsUncached(t, f.store, history)
	f.addTurn(t, 4)
	second := assertSameAsUncached(t, f.store, history)
	if len(second) != len(first)+2 {
		t.Fatalf("second read has %d items, want %d", len(second), len(first)+2)
	}
	// The prefix is shared with the first read, not re-expanded.
	for index := range first {
		if &first[index][0] != &second[index][0] {
			t.Fatalf("item %d was re-expanded instead of reused", index)
		}
	}
}

func TestInputHistoryResultIsIndependentOfLaterReads(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	history := f.store.NewInputHistory()
	first, err := history.Read()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := append([][]byte(nil), first...)
	f.addTurn(t, 1)
	if _, err := history.Read(); err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !equalJSONLines(first, snapshot) {
		t.Fatalf("an earlier result changed after a later read: %q", first)
	}
	// Appending to a returned slice must not corrupt the retained items.
	_ = append(first[:1], []byte("overwritten"))
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryDropsItemsAfterCompaction(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
	}
	history := f.store.NewInputHistory()
	before := assertSameAsUncached(t, f.store, history)
	result, err := f.store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"})
	if err != nil || !result.Compacted {
		t.Fatalf("CompactHistory() = %+v, %v", result, err)
	}
	after := assertSameAsUncached(t, f.store, history)
	if len(after) >= len(before) {
		t.Fatalf("history has %d items after compaction, want fewer than %d", len(after), len(before))
	}
	// New turns after the compaction still match.
	f.addTurn(t, 3)
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryDropsItemsAfterReset(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
	}
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	if err := f.store.Reset(); err != nil {
		t.Fatal(err)
	}
	if items := assertSameAsUncached(t, f.store, history); len(items) != 0 {
		t.Fatalf("history after reset has %d items", len(items))
	}
	f.addTurn(t, 9)
	items := assertSameAsUncached(t, f.store, history)
	if len(items) != 2 || !strings.Contains(string(items[0]), "question 9") {
		t.Fatalf("history after reset and new turn = %q", items)
	}
}

func TestInputHistoryDropsItemsAfterFrontCompaction(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
	}
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)

	var secondUser int64 = -1
	if err := f.store.ReadRecords(func(offset int64, line []byte) error {
		if secondUser < 0 && strings.Contains(string(line), "question 1") {
			secondUser = offset
		}
		return nil
	}); err != nil || secondUser <= 0 {
		t.Fatalf("locating second turn: offset=%d err=%v", secondUser, err)
	}
	// Front compaction keeps the prefix before the target message.
	if _, err := f.store.FrontCompactFromMessage(fmt.Sprintf("session-user-o%d", secondUser), "permanent"); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if len(items) == 0 || strings.Contains(joinItems(items), "question 1") {
		t.Fatalf("front-compacted history still holds removed turns: %q", items)
	}
}

func TestInputHistoryDetectsAnExternallyRewrittenTranscript(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
	}
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)

	// Replace the file with different content of the SAME size, and restore the
	// old modification time: only the content hash can notice this.
	path := filepath.Join(f.dir, "messages.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := []byte(strings.Replace(string(original), "question 2", "QUESTION 2", 1))
	if len(rewritten) != len(original) || string(rewritten) == string(original) {
		t.Fatal("test setup did not produce a same-size rewrite")
	}
	if err := os.WriteFile(path, rewritten, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if !strings.Contains(joinItems(items), "QUESTION 2") {
		t.Fatalf("history did not notice the rewritten transcript: %q", items)
	}
}

// The tail hash only covers the last historyCheckpointBytes, so a rewrite that
// changes an early record and keeps the size and the tail identical is invisible
// to it. The journal generation, file identity and mtime must catch that.
func TestInputHistoryDropsItemsAfterAnInProcessRewriteThatKeepsTheTail(t *testing.T) {
	f := newMemoFixture(t)
	big := func(fill byte) []byte {
		// The distinguishing bytes are first, far outside the hashed tail window.
		return []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat(string(fill), 8) + strings.Repeat("x", int(historyCheckpointBytes)+4096) + `"}]}`)
	}
	tail := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"shared tail"}]}`)
	if err := f.journal.Append(big('a'), tail); err != nil {
		t.Fatal(err)
	}
	history := f.store.NewInputHistory()
	first := assertSameAsUncached(t, f.store, history)
	if !strings.Contains(string(first[0]), "aaaa") {
		t.Fatal("setup: first record is not the 'a' record")
	}
	if err := f.store.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := f.journal.Append(big('b'), tail); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if !strings.Contains(string(items[0]), "bbbb") || strings.Contains(string(items[0]), "aaaa") {
		t.Fatal("history kept the pre-reset record")
	}
}

func TestInputHistoryDetectsAReplacedTranscriptFile(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	f.addTurn(t, 1)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)

	path := filepath.Join(f.dir, "messages.jsonl")
	replacement := filepath.Join(f.dir, "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"other"}]}`+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if len(items) != 1 || !strings.Contains(string(items[0]), "other") {
		t.Fatalf("history after file replacement = %q", items)
	}
}

func TestInputHistoryRepairsAPartialTrailingRecord(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)

	path := filepath.Join(f.dir, "messages.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"message","role":"user","content":[{"type":"input_te`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if len(items) != 2 {
		t.Fatalf("history with a torn tail has %d items, want 2", len(items))
	}
	f.addTurn(t, 1)
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryReportsAnInvalidRecordWithoutPoisoningTheMemo(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	if err := f.journal.Append([]byte(`{"type":"response.created"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Read(); err == nil || !strings.Contains(err.Error(), "unsupported response event") {
		t.Fatalf("Read() error = %v, want unsupported response event", err)
	}
	// A failed read changes nothing; the same error repeats, and the memo is intact.
	if _, err := history.Read(); err == nil {
		t.Fatal("second Read() succeeded after an invalid record")
	}
	if err := f.store.Reset(); err != nil {
		t.Fatal(err)
	}
	f.addTurn(t, 5)
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryAppliesTheCompactionOverlayOnTopOfTheMemo(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
	}
	if result, err := f.store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"}); err != nil || !result.Compacted {
		t.Fatalf("CompactHistory() = %+v, %v", result, err)
	}
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	f.addTurn(t, 3)
	f.addTurn(t, 4)
	items := assertSameAsUncached(t, f.store, history)
	if !strings.Contains(joinItems(items), "question 4") {
		t.Fatalf("history is missing the newest turn: %q", items)
	}
}

func TestInputHistoryReleaseDropsTheRetainedItems(t *testing.T) {
	f := newMemoFixture(t)
	f.addTurn(t, 0)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	history.Release()
	if history.memo.valid || history.memo.items != nil {
		t.Fatal("Release() kept the retained items")
	}
	assertSameAsUncached(t, f.store, history)
}

func TestNilInputHistoryReturnsAnError(t *testing.T) {
	var history *InputHistory
	if _, err := history.Read(); err == nil {
		t.Fatal("Read() on a nil InputHistory returned no error")
	}
	history.Release()
}

func joinItems(items [][]byte) string {
	parts := make([]string, len(items))
	for index, item := range items {
		parts[index] = string(item)
	}
	return strings.Join(parts, "\n")
}

func (f memoFixture) compact(t *testing.T, keep int) {
	t.Helper()
	result, err := f.store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: keep, BackupMode: "append"})
	if err != nil || !result.Compacted {
		t.Fatalf("CompactHistory() = %+v, %v", result, err)
	}
}

func (f memoFixture) compactionCachePath() string {
	return filepath.Join(f.dir, "staging", "compaction-cache.json")
}

func TestInputHistoryReusesTheParsedCompactionCache(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 5; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 3)
	history := f.store.NewInputHistory()
	first := assertSameAsUncached(t, f.store, history)
	f.addTurn(t, 5)
	second := assertSameAsUncached(t, f.store, history)
	if len(second) <= len(first) {
		t.Fatalf("second read has %d items, want more than %d", len(second), len(first))
	}
	for index := range first {
		if &first[index][0] != &second[index][0] {
			t.Fatalf("compacted item %d was re-parsed instead of reused", index)
		}
	}
	// A plain uncached read still returns private copies.
	one, _ := f.store.ReadInputItems()
	two, _ := f.store.ReadInputItems()
	if &one[0][0] == &two[0][0] {
		t.Fatal("ReadInputItems() shares item bytes between calls")
	}
}

func TestInputHistoryReloadsTheCompactionCacheAfterASecondCompaction(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 6; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 4)
	history := f.store.NewInputHistory()
	before := assertSameAsUncached(t, f.store, history)
	f.addTurn(t, 6)
	f.compact(t, 2)
	after := assertSameAsUncached(t, f.store, history)
	if len(after) >= len(before)+2 {
		t.Fatalf("history did not shrink after the second compaction: %d -> %d", len(before), len(after))
	}
	f.addTurn(t, 7)
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryDropsTheCompactionCacheAfterReset(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 2)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	if err := f.store.Reset(); err != nil {
		t.Fatal(err)
	}
	if items := assertSameAsUncached(t, f.store, history); len(items) != 0 {
		t.Fatalf("history after reset has %d items", len(items))
	}
}

func TestInputHistoryNoticesACompactionCacheReplacedOutsideTheStore(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 2)
	history := f.store.NewInputHistory()
	before := assertSameAsUncached(t, f.store, history)

	// Same-size rewrite with the old mtime: only the file identity can notice it,
	// so write a new file and rename it over the cache.
	path := f.compactionCachePath()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, item := range before {
		if strings.Contains(string(item), "question") {
			target = string(item)
			break
		}
	}
	if target == "" {
		t.Fatal("setup: no compacted item holds a question")
	}
	marker := strings.Index(string(original), "question")
	if marker < 0 {
		t.Fatalf("setup: compaction cache has no question text: %s", original)
	}
	rewritten := []byte(string(original[:marker]) + "QUESTION" + string(original[marker+len("question"):]))
	replacement := path + ".new"
	if err := os.WriteFile(replacement, rewritten, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, time.Now(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	items := assertSameAsUncached(t, f.store, history)
	if !strings.Contains(joinItems(items), "QUESTION") {
		t.Fatalf("history did not notice the replaced compaction cache: %q", items)
	}
}

func TestInputHistoryFollowsTheCompactionCacheBackupWhenThePrimaryIsRemoved(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 2)
	history := f.store.NewInputHistory()
	want := assertSameAsUncached(t, f.store, history)
	if err := os.Remove(f.compactionCachePath()); err != nil {
		t.Fatal(err)
	}
	got := assertSameAsUncached(t, f.store, history)
	if !equalJSONLines(got, want) {
		t.Fatalf("history changed when only the backup remained:\n got  %q\n want %q", got, want)
	}
	// With both files gone, the compacted prefix is gone too.
	if err := os.Remove(f.compactionCachePath() + ".backup"); err != nil {
		t.Fatal(err)
	}
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryRejectsACorruptCompactionCacheLikeAFullRead(t *testing.T) {
	f := newMemoFixture(t)
	for index := 0; index < 4; index++ {
		f.addTurn(t, index)
	}
	f.compact(t, 2)
	history := f.store.NewInputHistory()
	assertSameAsUncached(t, f.store, history)
	if err := os.WriteFile(f.compactionCachePath(), []byte("{not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	assertSameAsUncached(t, f.store, history)
	if err := os.WriteFile(f.compactionCachePath()+".backup", []byte("{not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	assertSameAsUncached(t, f.store, history)
}

func TestInputHistoryWithoutACompactionCachePathMatchesAFullRead(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	f := memoFixture{dir: dir, journal: journal, store: store}
	history := store.NewInputHistory()
	for index := 0; index < 3; index++ {
		f.addTurn(t, index)
		assertSameAsUncached(t, store, history)
	}
}
