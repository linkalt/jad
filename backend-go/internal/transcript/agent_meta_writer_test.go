package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var testBatchID = regexp.MustCompile(`"` + SessionInputBatchIDField + `":"[0-9a-f]{32}"`)

func stampedStore(t *testing.T, root string, at time.Time) (*Journal, *TurnStore) {
	t.Helper()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	store.options.UserTimestamp = true
	store.now = func() time.Time { return at }
	return journal, store
}

func transcriptLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// Every user message of a batch gets the batch's one stored time, last, after the steer marker;
// other input items (tool outputs, developer and system messages) get nothing. With the
// setting off the records are the bytes of a build without the field.
func TestUserMessagesGetTheStoredTimeOfTheirBatch(t *testing.T) {
	at := time.Date(2026, 11, 13, 7, 1, 2, 345_678_000, time.FixedZone("x", 3600))
	journal, store := stampedStore(t, t.TempDir(), at)
	user := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}`)
	empty := []byte(`{"type":"message","role":"user","content":[]}`)
	output := []byte(`{"type":"function_call_output","call_id":"c","output":"x"}`)
	developer := []byte(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"hook context"}]}`)
	system := []byte(`{"type":"message","role":"system","content":"stop"}`)
	if err := store.AppendInputItems(user, empty, output, developer); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendSteerInputItemsAt(user, system); err != nil {
		t.Fatal(err)
	}
	stamp := `,"` + MetaField + `":{"` + MetaStoredAt + `":"2026-11-13T06:01:02.345Z"}}`
	lines := transcriptLines(t, journal.path)
	if len(lines) != 6 {
		t.Fatalf("lines = %d", len(lines))
	}
	for index, stamped := range []bool{true, true, false, false, true, false} {
		if strings.HasSuffix(lines[index], stamp) != stamped || strings.Count(lines[index], MetaField) != map[bool]int{true: 1, false: 0}[stamped] {
			t.Errorf("record %d (stamped %v): %s", index, stamped, lines[index])
		}
	}
	if !strings.HasSuffix(lines[4], `,"`+SessionInputSteerField+`":true`+stamp) {
		t.Errorf("steered record order: %s", lines[4])
	}
	if !recordIsSteered([]byte(lines[4])) {
		t.Error("the steer marker is not read with the meta after it")
	}
	if value, ok := MetaValue([]byte(lines[0]), MetaStoredAt, 0); !ok || string(value) != `"2026-11-13T06:01:02.345Z"` {
		t.Errorf("MetaValue = %s, %v", value, ok)
	}

	// Off: the same input written by a store without the setting is every record without the
	// trailing member, byte for byte (batch ids masked: they are random).
	offJournal, off := newTestTurnStore(t, t.TempDir(), DurabilityProcess, ResponseMetadataMinimal)
	if err := off.AppendInputItems(user, empty, output, developer); err != nil {
		t.Fatal(err)
	}
	if _, err := off.AppendSteerInputItemsAt(user, system); err != nil {
		t.Fatal(err)
	}
	offLines := transcriptLines(t, offJournal.path)
	for index, line := range lines {
		dropped, err := DropTrailingMeta([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if testBatchID.ReplaceAllString(string(dropped), "") != testBatchID.ReplaceAllString(offLines[index], "") || strings.Contains(offLines[index], MetaField) {
			t.Fatalf("record %d: on without meta %s, off %s", index, dropped, offLines[index])
		}
	}
	onHistory, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	offHistory, err := off.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(onHistory) != fmt.Sprint(offHistory) {
		t.Fatalf("history differs:\n%q\n%q", onHistory, offHistory)
	}
}

// strip-local --meta-only turns a transcript written with the setting into one that reads, line
// for line after json.Compact, as the transcript written without it; size and offsets stay.
func TestStripMetaOnlyReadsAsTheTranscriptWrittenWithoutTheSetting(t *testing.T) {
	at := time.Date(2026, 11, 13, 7, 1, 2, 0, time.UTC)
	onJournal, on := stampedStore(t, t.TempDir(), at)
	offJournal, off := newTestTurnStore(t, t.TempDir(), DurabilityProcess, ResponseMetadataMinimal)
	for _, store := range []*TurnStore{on, off} {
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":"a"}`), []byte(`{"type":"function_call_output","call_id":"c","output":"x"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendSteerInputItemsAt([]byte(`{"type":"message","role":"user","content":"steer"}`)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(onJournal.path)
	if counts, err := StripLocalFields(onJournal.path, true, true); err != nil || counts != (LocalStripCounts{Records: 2, Meta: 2}) {
		t.Fatalf("strip = %+v, %v", counts, err)
	}
	after, _ := os.ReadFile(onJournal.path)
	if len(after) != len(before) {
		t.Fatalf("size %d, want %d", len(after), len(before))
	}
	stripped, plain := transcriptLines(t, onJournal.path), transcriptLines(t, offJournal.path)
	for index, line := range stripped {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(line)); err != nil {
			t.Fatal(err)
		}
		if testBatchID.ReplaceAllString(compact.String(), "") != testBatchID.ReplaceAllString(plain[index], "") {
			t.Fatalf("record %d: %s, want %s", index, compact.String(), plain[index])
		}
	}
	if counts, err := StripLocalFields(onJournal.path, true, true); err != nil || counts.Records != 0 {
		t.Fatalf("second strip = %+v, %v", counts, err)
	}
}

// A store reopened on a transcript with meta records (and a torn record at its tail) recovers
// without touching the complete records, and front compaction from the second message of a
// stamped batch still cuts at the batch's first record.
func TestRecoveryAndFrontCompactionWithMeta(t *testing.T) {
	root := t.TempDir()
	journal, store := stampedStore(t, root, time.Now())
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":"keep"}`)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"r0","status":"completed","output":[]}}`)); err != nil {
		t.Fatal(err)
	}
	complete, _ := os.ReadFile(journal.path)
	torn := append(append([]byte(nil), complete...), `{"type":"message","role":"user","content":"x","__agent_meta":{"stored_`...)
	if err := os.WriteFile(journal.path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	reopenedJournal, reopened := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	reopened.options.UserTimestamp = true
	if _, err := reopened.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got, _ := os.ReadFile(reopenedJournal.path); !bytes.Equal(got, complete) {
		t.Fatalf("recovery left:\n%s\nwant:\n%s", got, complete)
	}

	fstore, fjournal, paths := newFrontCompactTestStore(t)
	fstore.options.UserTimestamp = true
	if err := fstore.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"keep prefix"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := fstore.AppendInputItems(
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"batch one"}]}`),
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"batch two"}]}`)); err != nil {
		t.Fatal(err)
	}
	var batchStart, target int64 = -1, -1
	if err := fjournal.ReadRecords(func(offset int64, line []byte) error {
		if !strings.Contains(string(line), MetaStoredAt) {
			return fmt.Errorf("record without the stored time: %s", line)
		}
		if strings.Contains(string(line), "batch one") {
			batchStart = offset
		}
		if strings.Contains(string(line), "batch two") {
			target = offset
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(paths.messages)
	if _, err := fstore.FrontCompactFromMessage(fmt.Sprintf("session-user-o%d", target), "permanent"); err != nil {
		t.Fatal(err)
	}
	if current, _ := os.ReadFile(paths.messages); string(current) != string(original[:batchStart]) {
		t.Fatalf("front compaction kept %q, want %q", current, original[:batchStart])
	}
}

// The stored time is taken under the store's lock: concurrent appends race-free (go test -race),
// and every record of one batch has its batch's value.
func TestConcurrentAppendsStampEachBatchOnce(t *testing.T) {
	journal, store := newTestTurnStore(t, t.TempDir(), DurabilityProcess, ResponseMetadataMinimal)
	store.options.UserTimestamp = true
	var calls int
	store.now = func() time.Time {
		calls++ // under s.mu; -race reports it otherwise
		return time.Date(2026, 1, 1, 0, 0, 0, calls*int(time.Millisecond), time.UTC)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":"a"}`), []byte(`{"type":"message","role":"user","content":"b"}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	byBatch := map[string]string{}
	for _, line := range transcriptLines(t, journal.path) {
		batch := testBatchID.FindString(line)
		value, ok := MetaValue([]byte(line), MetaStoredAt, 0)
		if !ok {
			t.Fatalf("no stored time: %s", line)
		}
		if previous, seen := byBatch[batch]; seen && previous != string(value) {
			t.Fatalf("batch %s has %s and %s", batch, previous, value)
		}
		byBatch[batch] = string(value)
	}
	if len(byBatch) != 8 || calls != 8 {
		t.Fatalf("batches %d, clock calls %d; want 8 and 8", len(byBatch), calls)
	}
}
