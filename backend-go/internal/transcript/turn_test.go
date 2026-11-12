package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestTurnStore(t *testing.T, root string, durability DurabilityMode, metadata ResponseMetadataMode) (*Journal, *TurnStore) {
	t.Helper()
	journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), durability)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		MaxItemBytes:     1024 * 1024,
		ResponseMetadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return journal, store
}

func outputItemEvent(index int64, item string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item))
}

func TestCommitObserverRunsAfterTranscriptAppendBeforeStagingCleanup(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_observer","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)
	turn, err := store.BeginTurn("turn-observer")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
		t.Fatal(err)
	}
	partialPath := turn.PartialContentPath()
	if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := turn.CommitWithObserver(completed, func(commit CommitResult) {
		called = true
		if commit.Bytes <= 0 || commit.LineHash == "" {
			t.Errorf("observer commit result = %+v", commit)
		}
		if transcript, err := os.ReadFile(journal.path); err != nil || len(transcript) == 0 {
			t.Errorf("observer ran before canonical append: bytes=%d error=%v", len(transcript), err)
		}
		if content, err := os.ReadFile(partialPath); err != nil || string(content) != "partial" {
			t.Errorf("partial cache unavailable during observer: content=%q error=%v", content, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("commit observer was not called")
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Errorf("committed staging still exists after observer: err=%v", err)
	}
}

func TestCommitObserverPanicDefersStagingCleanupWithoutFailingCommittedResponse(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_observer_panic","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)
	turn, err := store.BeginTurn("turn-observer-panic")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.CommitWithObserver(completed, func(CommitResult) { panic("observer failure") }); err != nil {
		t.Fatalf("committed response reported observer failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "staging", "turn-observer-panic", manifestFilename)); err != nil {
		t.Fatalf("committed staging was not preserved for deferred cleanup: %v", err)
	}
	if records, err := os.ReadFile(journal.path); err != nil || len(records) == 0 {
		t.Fatalf("canonical response was not committed: bytes=%d error=%v", len(records), err)
	}
}

func TestTurnStagesOutputByIndexAndCommitsCanonicalLineIdempotently(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	messageItem := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}`
	functionItem := `{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}`
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_test","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":10,"output_tokens":4},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}]}}`)
	// This Node-generated JSON.stringify fixture uses the canonical response
	// fixture shape from the removed TypeScript backend (tag legacy-final).
	golden, err := os.ReadFile(filepath.Join("testdata", "messages-canonical.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	wantLine := string(golden)

	turn, err := store.BeginTurn("turn-out-of-order")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(1, functionItem)); err != nil {
		t.Fatalf("stage function item: %v", err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, messageItem)); err != nil {
		t.Fatalf("stage message item: %v", err)
	}

	turnDir := filepath.Join(root, "staging", "turn-out-of-order")
	staged, err := os.ReadFile(filepath.Join(turnDir, itemsFilename))
	if err != nil {
		t.Fatal(err)
	}
	wantStaged := `{"output_index":1,"item":{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}}` + "\n" +
		`{"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}` + "\n"
	if string(staged) != wantStaged {
		t.Fatalf("staged items differ\n got: %s\nwant: %s", staged, wantStaged)
	}
	manifest, err := os.ReadFile(filepath.Join(turnDir, manifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	// Strict mode skips the per-item manifest write: the counters stay at their
	// Begin values until the ready manifest, and recovery ignores them.
	if got, want := string(manifest), `{"schema_version":1,"turn_id":"turn-out-of-order","status":"streaming","item_count":0,"next_sequence":0}`+"\n"; got != want {
		t.Fatalf("manifest = %s, want %s", got, want)
	}

	result, err := turn.Commit(completed)
	if err != nil {
		t.Fatalf("commit response: %v", err)
	}
	transcript, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(transcript) != wantLine {
		t.Fatalf("messages.jsonl differs\n got: %s\nwant: %s", transcript, wantLine)
	}
	wantHash := sha256.Sum256([]byte(wantLine))
	if result.LineHash != hex.EncodeToString(wantHash[:]) || result.Bytes != int64(len(wantLine)) || result.AlreadyPresent {
		t.Fatalf("unexpected commit result: %+v", result)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "staging")); err != nil || len(entries) != 0 {
		t.Fatalf("staging directory not cleaned: entries=%v error=%v", entries, err)
	}

	// A retry of an identical completed response is detected by its canonical
	// line hash and does not append a duplicate transcript record.
	retry, err := store.BeginTurn("turn-duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.StageOutputItem(outputItemEvent(0, messageItem)); err != nil {
		t.Fatal(err)
	}
	if err := retry.StageOutputItem(outputItemEvent(1, functionItem)); err != nil {
		t.Fatal(err)
	}
	retryResult, err := retry.Commit(completed)
	if err != nil {
		t.Fatalf("commit duplicate response: %v", err)
	}
	if !retryResult.AlreadyPresent || retryResult.LineHash != result.LineHash {
		t.Fatalf("duplicate commit result = %+v, want already present with hash %s", retryResult, result.LineHash)
	}
	transcript, err = os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(transcript) != wantLine {
		t.Fatalf("duplicate commit changed transcript: %s", transcript)
	}
}

func TestTurnMetadataFilteringAndUsageCompaction(t *testing.T) {
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	event := []byte(`{"type":"response.completed","session_turn":3,"session_step":2,"response":{"id":"resp-meta","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":99},"output_tokens_details":{"reasoning_tokens":3},"attribution":{"items":{"fc_x":{"input_tokens":5}}}},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"tools":[{"type":"function","name":"read"}],"vendor_field":{"10":"ten","2":"two","z":"<raw>"},"created_at":1790186909,"completed_at":1790186915}}`)

	for _, test := range []struct {
		mode ResponseMetadataMode
		want string
	}{
		{
			mode: ResponseMetadataMinimal,
			want: `{"type":"response.completed","session_turn":3,"session_step":2,"response":{"id":"resp-meta","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":2}},"created_at":1790186909,"completed_at":1790186915,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}` + "\n",
		},
		{
			mode: ResponseMetadataFull,
			want: `{"type":"response.completed","session_turn":3,"session_step":2,"response":{"id":"resp-meta","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":2}},"tools":[{"type":"function","name":"read"}],"vendor_field":{"2":"two","10":"ten","z":"<raw>"},"created_at":1790186909,"completed_at":1790186915,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}` + "\n",
		},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			root := t.TempDir()
			journal, store := newTestTurnStore(t, root, DurabilityProcess, test.mode)
			turn, err := store.BeginTurn("turn-meta")
			if err != nil {
				t.Fatal(err)
			}
			if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
				t.Fatal(err)
			}
			if _, err := turn.Commit(event); err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			got, err := os.ReadFile(journal.path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Fatalf("metadata mode %s line differs\n got: %s\nwant: %s", test.mode, got, test.want)
			}
			if strings.Contains(string(got), "attribution") || strings.Contains(string(got), "cache_write_tokens") || strings.Contains(string(got), "reasoning_tokens") {
				t.Fatalf("unwanted usage fields persisted: %s", got)
			}
		})
	}
}

func TestTurnRejectsInvalidIndexesAndOutputCountMismatch(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-validation")
	if err != nil {
		t.Fatal(err)
	}
	item := `{"type":"message","role":"assistant","content":[]}`
	if err := turn.StageOutputItem(outputItemEvent(1, item)); err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(1, item)); err == nil {
		t.Fatal("duplicate output_index was accepted")
	}
	// The one staged item has index 1 and the completed output has one item that differs
	// from it and shares no id: they are not paired by guessing.
	mismatched := []byte(`{"type":"response.completed","response":{"id":"resp-mismatch","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"other"}]}]}}`)
	if _, err := turn.Commit(mismatched); err == nil || !strings.Contains(err.Error(), "cannot be matched to staged output_index 1") {
		t.Fatalf("Commit() error = %v, want an unmatched item error", err)
	}
	if turn.manifest.Status != "streaming" {
		t.Fatalf("failed validation changed turn status to %q", turn.manifest.Status)
	}
}

func TestEmptyTurnCommitsEmptyOutput(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-empty")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Commit([]byte(`{"type":"response.completed","response":{"id":"resp-empty","status":"completed","output":[]}}`))
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	got, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"response.completed","response":{"id":"resp-empty","status":"completed","output":[]}}` + "\n"
	if string(got) != want {
		t.Fatalf("empty response line = %s, want %s", got, want)
	}
	if result.Bytes != int64(len(want)) || result.AlreadyPresent {
		t.Fatalf("unexpected empty response commit result: %+v", result)
	}
}

func TestLargeOutputItemIsStagedAndCommittedWithoutTruncation(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	text := strings.Repeat("x", 256*1024)
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
	turn, err := store.BeginTurn("turn-large-item")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
		t.Fatalf("StageOutputItem() error = %v", err)
	}
	completed := []byte(`{"type":"response.completed","response":{"id":"resp-large","status":"completed","output":[` + item + `]}}`)
	if _, err := turn.Commit(completed); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	transcript, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(transcript)) {
		t.Fatal("committed large transcript line is invalid JSON")
	}
	var event struct {
		Response struct {
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(transcript, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Response.Output) != 1 || len(event.Response.Output[0].Content) != 1 || event.Response.Output[0].Content[0].Text != text {
		t.Fatal("large output item changed during staging or commit")
	}
}

func TestTurnOutputItemLimitIsEnforced(t *testing.T) {
	root := t.TempDir()
	journal, err := NewJournal(filepath.Join(root, "messages.jsonl"), DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(root, "staging"), TurnStoreOptions{
		MaxItemBytes:     4,
		ResponseMetadata: ResponseMetadataMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.BeginTurn("turn-limit")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, `{"type":"message"}`)); err == nil {
		t.Fatal("oversized output item was accepted")
	}
	manifest, err := os.ReadFile(filepath.Join(root, "staging", "turn-limit", manifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var parsed TurnManifest
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ItemCount != 0 || parsed.Status != "streaming" {
		t.Fatalf("oversized item changed manifest: %+v", parsed)
	}
}

func TestTurnStageIndexOrderAndManifestStatusShape(t *testing.T) {
	root := t.TempDir()
	_, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-stage-format")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.StageOutputItem(outputItemEvent(0, `{"type":"message"}`)); err != nil {
		t.Fatal(err)
	}
	indexContent, err := os.ReadFile(filepath.Join(root, "staging", "turn-stage-format", indexFilename))
	if err != nil {
		t.Fatal(err)
	}
	var indexRecord struct {
		OutputIndex int64 `json:"output_index"`
		Offset      int64 `json:"offset"`
		Length      int64 `json:"length"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(indexContent), &indexRecord); err != nil {
		t.Fatal(err)
	}
	itemLine := `{"output_index":0,"item":{"type":"message"}}` + "\n"
	if !reflect.DeepEqual(indexRecord, struct {
		OutputIndex int64 `json:"output_index"`
		Offset      int64 `json:"offset"`
		Length      int64 `json:"length"`
	}{OutputIndex: 0, Offset: 0, Length: int64(len(itemLine))}) {
		t.Fatalf("staging index = %+v, want output_index=0, offset=0, length=%d", indexRecord, len(itemLine))
	}
}
