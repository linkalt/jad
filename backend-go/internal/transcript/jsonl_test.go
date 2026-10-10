package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendWritesCompactJSONLRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}

	first := []byte("{ \n \"type\" : \"message\", \"role\" : \"user\", \"content\" : [{\"type\":\"input_text\", \"text\":\"<hello & café>\"}] \n}")
	second := []byte(`{"type":"function_call_output","call_id":"call_1","output":"done"}`)
	if err := journal.Append(first, second); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"<hello & café>\"}]}\n" +
		"{\"type\":\"function_call_output\",\"call_id\":\"call_1\",\"output\":\"done\"}\n"
	if string(got) != want {
		t.Fatalf("transcript bytes differ\n got: %s\nwant: %s", got, want)
	}
}

func TestAppendRepairsIncompleteTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	complete := []byte("{\"type\":\"message\",\"role\":\"user\"}\n")
	partial := []byte(`{"type":"response.completed","response":`)
	if err := os.WriteFile(path, append(append([]byte(nil), complete...), partial...), 0o600); err != nil {
		t.Fatal(err)
	}

	journal, err := NewJournal(path, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}
	appended := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`)
	if err := journal.Append(appended); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), complete...), append(append([]byte(nil), appended...), '\n')...)
	if !bytes.Equal(got, want) {
		t.Fatalf("repaired transcript bytes differ\n got: %s\nwant: %s", got, want)
	}
}

func TestAppendRepairsLargeIncompleteTailAcrossScanChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	complete := []byte("{\"type\":\"message\",\"role\":\"user\"}\n")
	partial := bytes.Repeat([]byte("x"), 2*64*1024+17)
	if err := os.WriteFile(path, append(append([]byte(nil), complete...), partial...), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	appended := []byte(`{"type":"message","role":"assistant"}`)
	if err := journal.Append(appended); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), complete...), append(append([]byte(nil), appended...), '\n')...)
	if !bytes.Equal(got, want) {
		t.Fatalf("large tail repair mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

func TestAppendDropsValidButUnterminatedTailLikeTypeScriptStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"message","role":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append([]byte(`{"type":"message","role":"assistant"}`)); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"type\":\"message\",\"role\":\"assistant\"}\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("transcript = %s, want %s", got, want)
	}
}

func TestAppendValidatesWholeBatchBeforeRepairOrWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	original := []byte("{\"type\":\"message\"}\n{partial")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(
		[]byte(`{"type":"message","role":"user"}`),
		[]byte(`{"type":"message"} trailing`),
	); err == nil {
		t.Fatal("Append() succeeded with invalid second record")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("invalid batch modified transcript\n got: %s\nwant: %s", got, original)
	}
}

func TestAppendPreservesCanonicalCompletedEventLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	journal, err := NewJournal(path, DurabilityStrict)
	if err != nil {
		t.Fatal(err)
	}

	// This fixture follows the canonical envelope/output ordering used by
	// the removed TypeScript backend (tag legacy-final) and its response.completed commit test.
	line := []byte(`{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"resp_test","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":10,"output_tokens":4},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}]}}`)
	if err := journal.Append(line); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), line...), '\n'); !bytes.Equal(got, want) {
		t.Fatalf("canonical line changed\n got: %s\nwant: %s", got, want)
	}
}

func TestEmptyAppendDoesNotCreateTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty append created transcript; stat error = %v", err)
	}
}

func TestConcurrentAppendsProduceCompleteRecords(t *testing.T) {
	const recordCount = 64
	path := filepath.Join(t.TempDir(), "messages.jsonl")
	journal, err := NewJournal(path, DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errors := make(chan error, recordCount)
	for index := 0; index < recordCount; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			record := []byte(fmt.Sprintf(`{"type":"message","index":%d}`, index))
			errors <- journal.Append(record)
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("Append() error = %v", err)
		}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != recordCount {
		t.Fatalf("got %d transcript lines, want %d", len(lines), recordCount)
	}
	seen := make(map[int]bool, recordCount)
	for _, line := range lines {
		var record struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid transcript line %q: %v", line, err)
		}
		if record.Type != "message" || seen[record.Index] {
			t.Fatalf("unexpected or duplicate record: %+v", record)
		}
		seen[record.Index] = true
	}
	if len(seen) != recordCount {
		t.Fatalf("got %d distinct records, want %d", len(seen), recordCount)
	}
}

func TestNewJournalRejectsUnsupportedDurability(t *testing.T) {
	if _, err := NewJournal(filepath.Join(t.TempDir(), "messages.jsonl"), "unknown"); err == nil {
		t.Fatal("NewJournal() succeeded with unsupported durability")
	}
}
