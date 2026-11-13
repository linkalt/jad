package transcript

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPersistInputItemMatchesNodeGeneratedMessagesJSONLFixture(t *testing.T) {
	item := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<hello & café>"}],"future":{"10":"ten","2":"two","value":9007199254740993}}`)
	canonical, err := canonicalJSON(item)
	if err != nil {
		t.Fatal(err)
	}
	got, err := persistInputItem(canonical, "0123456789abcdef0123456789abcdef", 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "messages-input-batch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("persisted input line differs from Node-generated golden\n got: %s\nwant: %s", got, want)
	}
}

func TestAppendInputItemsAddsOneBatchIDAndReadStripsMetadata(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityStrict, ResponseMetadataMinimal)
	first := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<hello & café>"}],"future":{"10":"ten","2":"two","value":9007199254740993}}`)
	second := []byte(`{"type":"function_call_output","call_id":"call_1","output":"result"}`)
	if err := store.AppendInputItems(first, second); err != nil {
		t.Fatalf("AppendInputItems() error = %v", err)
	}

	transcript, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(transcript), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d persisted input records, want 2: %s", len(lines), transcript)
	}
	var firstRecord map[string]json.RawMessage
	var secondRecord map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &firstRecord); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &secondRecord); err != nil {
		t.Fatal(err)
	}
	batchID, ok := stringValue(firstRecord[SessionInputBatchIDField])
	if !ok || len(batchID) != 32 {
		t.Fatalf("invalid batch ID %q", batchID)
	}
	if _, err := hex.DecodeString(batchID); err != nil {
		t.Fatalf("batch ID is not lowercase hex: %q", batchID)
	}
	if got, ok := stringValue(secondRecord[SessionInputBatchIDField]); !ok || got != batchID {
		t.Fatalf("second batch ID = %q, want %q", got, batchID)
	}
	if got := string(firstRecord[SessionInputBatchIndexField]); got != "0" {
		t.Errorf("first batch index = %s, want 0", got)
	}
	if got := string(secondRecord[SessionInputBatchIndexField]); got != "1" {
		t.Errorf("second batch index = %s, want 1", got)
	}
	firstFields, err := objectFields([]byte(lines[0]))
	if err != nil {
		t.Fatal(err)
	}
	fieldNames := make([]string, len(firstFields))
	for index, field := range firstFields {
		fieldNames[index] = field.key
	}
	wantFieldNames := []string{"type", "role", "content", "future", SessionInputBatchIDField, SessionInputBatchIndexField}
	if !reflect.DeepEqual(fieldNames, wantFieldNames) {
		t.Fatalf("persisted field order = %v, want %v", fieldNames, wantFieldNames)
	}

	gotItems, err := store.ReadInputItems()
	if err != nil {
		t.Fatalf("ReadInputItems() error = %v", err)
	}
	wantFirst := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<hello & café>"}],"future":{"2":"two","10":"ten","value":9007199254740992}}`)
	wantSecond := []byte(`{"type":"function_call_output","call_id":"call_1","output":"result"}`)
	if len(gotItems) != 2 || string(gotItems[0]) != string(wantFirst) || string(gotItems[1]) != string(wantSecond) {
		t.Fatalf("ReadInputItems() = %q, want [%q %q]", gotItems, wantFirst, wantSecond)
	}
}

func TestReadInputItemsExpandsCompletedResponsesAndStripsBatchMetadata(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	records := [][]byte{
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"__agent_session_input_batch_id":"batch-x","__agent_session_input_batch_index":0}`),
		[]byte(`{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`),
		[]byte(`{"type":"function_call_output","call_id":"call_1","output":"found"}`),
	}
	if err := journal.Append(records...); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReadInputItems()
	if err != nil {
		t.Fatalf("ReadInputItems() error = %v", err)
	}
	want := [][]byte{
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
		[]byte(`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}`),
		[]byte(`{"type":"function_call_output","call_id":"call_1","output":"found"}`),
	}
	if !equalJSONLines(got, want) {
		t.Fatalf("ReadInputItems() = %q, want %q", got, want)
	}
}

func TestReadInputItemsPreservesLiteralUnicodeSeparators(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	text := "before\u2028middle\u2029after"
	inputLine := []byte(`{"type":"function_call_output","call_id":"call_unicode_separators","output":"` + text + `"}`)
	completedLine := []byte(`{"type":"response.completed","response":{"id":"resp_unicode_separators","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}]}}`)
	transcript := append(append(append(append([]byte(nil), inputLine...), '\n'), completedLine...), '\n')
	if !bytes.Contains(transcript, []byte("\u2028")) || !bytes.Contains(transcript, []byte("\u2029")) {
		t.Fatal("test transcript does not contain literal U+2028 and U+2029 characters")
	}
	if err := os.WriteFile(journal.path, transcript, 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := store.ReadInputItems()
	if err != nil {
		t.Fatalf("ReadInputItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("ReadInputItems() returned %d items, want 2: %q", len(items), items)
	}
	var functionOutput struct {
		Type   string `json:"type"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(items[0], &functionOutput); err != nil {
		t.Fatalf("decode function output: %v", err)
	}
	if functionOutput.Type != "function_call_output" || functionOutput.Output != text {
		t.Fatalf("function output = %+v, want output %q", functionOutput, text)
	}
	var assistantMessage struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(items[1], &assistantMessage); err != nil {
		t.Fatalf("decode assistant message: %v", err)
	}
	if assistantMessage.Type != "message" || len(assistantMessage.Content) != 1 || assistantMessage.Content[0].Type != "output_text" || assistantMessage.Content[0].Text != text {
		t.Fatalf("assistant message = %+v, want output text %q", assistantMessage, text)
	}
}

func TestAppendInputItemsRejectsReservedFieldsAndResponseEventsBeforeWriting(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	valid := []byte(`{"type":"message","role":"user","content":[]}`)
	reserved := []byte(`{"type":"message","__agent_session_input_batch_index":0}`)
	responseEvent := []byte(`{"type":"response.output_text.delta","delta":"x"}`)
	for _, batch := range [][][]byte{{valid, reserved}, {responseEvent}} {
		if err := store.AppendInputItems(batch...); err == nil {
			t.Errorf("AppendInputItems(%q) succeeded, want validation error", batch)
		}
	}
	if _, err := os.Stat(journal.path); !os.IsNotExist(err) {
		t.Fatalf("invalid input batch touched messages.jsonl; stat error = %v", err)
	}
}

func TestAppendInputItemsRejectsWritesDuringActiveResponseTurn(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	turn, err := store.BeginTurn("turn-active-input-lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[]}`)); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("AppendInputItems() error = %v, want active-turn error", err)
	}
	if err := turn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[]}`)); err != nil {
		t.Fatalf("AppendInputItems() after turn close error = %v", err)
	}
	if _, err := os.Stat(journal.path); err != nil {
		t.Fatal(err)
	}
}

func TestAppendEmptyInputBatchDoesNotCreateTranscript(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	if err := store.AppendInputItems(); err != nil {
		t.Fatalf("AppendInputItems() error = %v", err)
	}
	if _, err := os.Stat(journal.path); !os.IsNotExist(err) {
		t.Fatalf("empty batch created transcript; stat error = %v", err)
	}
}

func TestReadInputItemsRejectsUnsupportedResponseEvents(t *testing.T) {
	root := t.TempDir()
	journal, store := newTestTurnStore(t, root, DurabilityProcess, ResponseMetadataMinimal)
	if err := journal.Append([]byte(`{"type":"response.failed","response":{"id":"resp_failed"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadInputItems(); err == nil || !strings.Contains(err.Error(), "unsupported response event") {
		t.Fatalf("ReadInputItems() error = %v, want unsupported response event", err)
	}
}

func equalJSONLines(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if string(left[index]) != string(right[index]) {
			return false
		}
	}
	return true
}
