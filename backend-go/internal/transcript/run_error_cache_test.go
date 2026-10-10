package transcript

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	visibleUserItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	imageOnlyItem   = `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`
	fileOnlyItem    = `{"type":"message","role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}`
)

func newRunErrorTestStore(t *testing.T) (*TurnStore, *Journal, string, string) {
	t.Helper()
	dir := t.TempDir()
	messages := filepath.Join(dir, "messages.jsonl")
	cache := filepath.Join(dir, "staging", "run-error-cache.json")
	journal, err := NewJournal(messages, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		MessagesBackupPath:  filepath.Join(dir, "messages.backup.jsonl"),
		CompactionCachePath: filepath.Join(dir, "staging", "compaction-cache.json"),
		HistoryCachePath:    filepath.Join(dir, "staging", "history-cache.json"),
		UsageCachePath:      filepath.Join(dir, "staging", "usage-cache.json"),
		CursorCachePath:     filepath.Join(dir, "staging", "cursor-cache.json"),
		RunErrorCachePath:   cache,
		StateContextPath:    filepath.Join(dir, "staging", "statecontext.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, journal, messages, cache
}

func runErrorEntry(turn int64, message string) RunErrorEntry {
	return RunErrorEntry{Turn: turn, MessageID: fmt.Sprintf("session-user-o%d", turn*100), TurnID: fmt.Sprintf("web-%d", turn), At: "2026-01-01T00:00:00.000Z", Message: message}
}

func TestRecordRunErrorWritesReadableEntries(t *testing.T) {
	store, _, _, cache := newRunErrorTestStore(t)
	if got := store.ReadRunErrors(); len(got) != 0 {
		t.Fatalf("a missing cache must read as empty, got %v", got)
	}
	want := runErrorEntry(1, "The run failed.")
	if err := store.RecordRunError(want, 50); err != nil {
		t.Fatal(err)
	}
	if got := store.ReadRunErrors(); !reflect.DeepEqual(got, []RunErrorEntry{want}) {
		t.Fatalf("ReadRunErrors() = %+v, want %+v", got, want)
	}
	encoded, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"schema_version":1,"errors":[{"turn":1,"message_id":"session-user-o100","turn_id":"web-1","at":"2026-01-01T00:00:00.000Z","message":"The run failed."}]}` + "\n"
	if string(encoded) != golden {
		t.Fatalf("cache bytes differ from the TypeScript shape\n got: %s\nwant: %s", encoded, golden)
	}
}

func TestRecordRunErrorOmitsAMissingMessageID(t *testing.T) {
	store, _, _, cache := newRunErrorTestStore(t)
	entry := runErrorEntry(1, "x")
	entry.MessageID = ""
	if err := store.RecordRunError(entry, 50); err != nil {
		t.Fatal(err)
	}
	encoded, _ := os.ReadFile(cache)
	if bytes.Contains(encoded, []byte("message_id")) {
		t.Fatalf("an entry with no message id must not write the key: %s", encoded)
	}
}

func TestRecordRunErrorReplacesTheSameTurnSortsAndCaps(t *testing.T) {
	store, _, _, _ := newRunErrorTestStore(t)
	for _, turn := range []int64{3, 1, 2} {
		if err := store.RecordRunError(runErrorEntry(turn, "first"), 2); err != nil {
			t.Fatal(err)
		}
	}
	got := store.ReadRunErrors()
	if len(got) != 2 || got[0].Turn != 2 || got[1].Turn != 3 {
		t.Fatalf("a cap of 2 must keep the newest two turns in order, got %+v", got)
	}
	if err := store.RecordRunError(runErrorEntry(3, "second"), 2); err != nil {
		t.Fatal(err)
	}
	got = store.ReadRunErrors()
	if len(got) != 2 || got[1].Message != "second" {
		t.Fatalf("a later failure of the same turn must replace the earlier one, got %+v", got)
	}
}

func TestRecordRunErrorRejectsANonPositiveCap(t *testing.T) {
	store, _, _, _ := newRunErrorTestStore(t)
	if err := store.RecordRunError(runErrorEntry(1, "x"), 0); err == nil {
		t.Fatal("a cap below one must be an error, not an unbounded file")
	}
}

func TestReadRunErrorsToleratesDamagedFiles(t *testing.T) {
	store, _, _, cache := newRunErrorTestStore(t)
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"not json":       `{`,
		"foreign schema": `{"schema_version":2,"errors":[{"turn":1,"turn_id":"a","at":"b","message":"c"}]}`,
		"no errors list": `{"schema_version":1}`,
		"empty":          ``,
	} {
		if err := os.WriteFile(cache, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := store.ReadRunErrors(); len(got) != 0 {
			t.Errorf("%s: want an empty cache, got %+v", name, got)
		}
	}
	mixed := `{"schema_version":1,"errors":[` +
		`{"turn":0,"turn_id":"a","at":"b","message":"turn below one"},` +
		`{"turn":2,"turn_id":"a","at":"b"},` +
		`{"turn":3,"message_id":7,"turn_id":"a","at":"b","message":"bad id type"},` +
		`{"turn":4,"turn_id":"a","at":"b","message":"ok"}]}`
	if err := os.WriteFile(cache, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	got := store.ReadRunErrors()
	if len(got) != 1 || got[0].Turn != 4 {
		t.Fatalf("only the valid entry must survive, got %+v", got)
	}
}

func TestRecordRunErrorWithoutAPathDoesNothing(t *testing.T) {
	_, store := newTestTurnStore(t, t.TempDir(), DurabilityStrict, ResponseMetadataMinimal)
	if err := store.RecordRunError(runErrorEntry(1, "x"), 50); err != nil {
		t.Fatalf("a store with no cache path must ignore failures, got %v", err)
	}
	if got := store.ReadRunErrors(); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestAppendInputItemsAtReportsTheVisibleUserMessageOffset(t *testing.T) {
	store, journal, messages, _ := newRunErrorTestStore(t)
	first, err := store.AppendInputItemsAt([]byte(visibleUserItem))
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasMessage || first.MessageOffset != 0 {
		t.Fatalf("first batch = %+v, want the message at offset 0", first)
	}
	size := fileSize(t, messages)
	second, err := store.AppendInputItemsAt([]byte(`{"type":"function_call_output","call_id":"c","output":"o"}`), []byte(visibleUserItem))
	if err != nil {
		t.Fatal(err)
	}
	// The tool output is the first record of the batch; the user message is the second.
	if !second.HasMessage || second.MessageOffset <= size {
		t.Fatalf("second batch = %+v, want an offset past the first %d bytes and the tool record", second, size)
	}
	lines := bytes.Split(mustRead(t, messages), []byte("\n"))
	var want int64
	for _, line := range lines[:2] {
		want += int64(len(line)) + 1
	}
	if second.MessageOffset != want {
		t.Fatalf("offset = %d, want %d (the start of the third record)", second.MessageOffset, want)
	}
	_ = journal
}

// The Go web-ui projection shows a user message that has an image and no text (the
// TypeScript one hides it), so the accepted-input event must point at it.
func TestAppendInputItemsAtReportsAnImageOnlyUserMessage(t *testing.T) {
	store, _, _, _ := newRunErrorTestStore(t)
	result, err := store.AppendInputItemsAt([]byte(imageOnlyItem))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasMessage {
		t.Fatalf("an image-only user message must be reported as visible, got %+v", result)
	}
}

// A file-only user message is shown too (webui.UserContent gives it a "[file: name]" text).
func TestAppendInputItemsAtReportsAFileOnlyUserMessage(t *testing.T) {
	store, _, _, _ := newRunErrorTestStore(t)
	result, err := store.AppendInputItemsAt([]byte(fileOnlyItem))
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasMessage {
		t.Fatalf("a file-only user message must be reported as visible, got %+v", result)
	}
}

func TestAppendInputItemsAtHasNoMessageForInputWithoutVisibleText(t *testing.T) {
	store, _, _, _ := newRunErrorTestStore(t)
	for name, item := range map[string]string{
		"empty text":    `{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`,
		"tool output":   `{"type":"function_call_output","call_id":"c","output":"o"}`,
		"system prompt": `{"type":"message","role":"system","content":[{"type":"input_text","text":"hi"}]}`,
		"system file":   `{"type":"message","role":"system","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}`,
	} {
		result, err := store.AppendInputItemsAt([]byte(item))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.HasMessage {
			t.Errorf("%s: must not report a visible message, got %+v", name, result)
		}
	}
}

func TestAppendInputItemsAtOffsetMatchesTheStartOfTheRecord(t *testing.T) {
	store, _, messages, _ := newRunErrorTestStore(t)
	for i := 0; i < 3; i++ {
		result, err := store.AppendInputItemsAt([]byte(visibleUserItem))
		if err != nil {
			t.Fatal(err)
		}
		data := mustRead(t, messages)
		if !bytes.HasPrefix(data[result.MessageOffset:], []byte(`{"type":"message"`)) {
			t.Fatalf("batch %d: offset %d does not start a record: %q", i, result.MessageOffset, data[result.MessageOffset:])
		}
	}
}

func TestResetCompactionAndFrontCompactionClearTheRunErrorCache(t *testing.T) {
	clearers := map[string]func(*testing.T, *TurnStore, int64){
		"reset": func(t *testing.T, store *TurnStore, _ int64) {
			if err := store.Reset(); err != nil {
				t.Fatal(err)
			}
		},
		"compaction": func(t *testing.T, store *TurnStore, _ int64) {
			if _, err := store.CompactHistory(CompactionOptions{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"}); err != nil {
				t.Fatal(err)
			}
		},
		"front compaction": func(t *testing.T, store *TurnStore, secondOffset int64) {
			if _, err := store.FrontCompactFromMessage(fmt.Sprintf("session-user-o%d", secondOffset), "permanent"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, clear := range clearers {
		t.Run(name, func(t *testing.T) {
			store, journal, _, cache := newRunErrorTestStore(t)
			var secondOffset int64
			for i, text := range []string{"one", "two"} {
				result, err := store.AppendInputItemsAt([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					secondOffset = result.MessageOffset
				}
				response := []byte(`{"type":"response.completed","response":{"id":"resp_` + text + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer ` + text + `"}]}]}}`)
				// Like the other maintenance tests, append the answered turn straight
				// to the journal instead of staging it through a response turn.
				if err := journal.Append(response); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.RecordRunError(runErrorEntry(1, "x"), 50); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(cache); err != nil {
				t.Fatalf("the cache must exist before %s: %v", name, err)
			}
			clear(t, store, secondOffset)
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("%s must delete the run-error cache, stat error = %v", name, err)
			}
			if got := store.ReadRunErrors(); len(got) != 0 {
				t.Fatalf("%s left entries behind: %+v", name, got)
			}
		})
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
