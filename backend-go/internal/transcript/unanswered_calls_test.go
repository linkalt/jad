package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestHasUnansweredToolCall(t *testing.T) {
	call := `{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}`
	output := `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	message := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	tests := []struct {
		name    string
		journal string
		want    bool
	}{
		{"no calls", message + "\n", false},
		{"answered", message + "\n" + call + "\n" + output + "\n", false},
		{"unanswered at the tail", message + "\n" + call + "\n", true},
		{"unanswered before a later answered call", call + "\n" + message + "\n" + `{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}` + "\n" + output + "\n", true},
		{"answer before the call does not count", output + "\n" + call + "\n", true},
		{"empty journal", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := HasUnansweredToolCall([]byte(test.journal)); got != test.want {
				t.Fatalf("HasUnansweredToolCall = %v, want %v", got, test.want)
			}
		})
	}
}

// TestHasUnansweredToolCallReadsCommittedResponses writes the journal through the real
// TurnStore, so the calls sit inside response.completed.response.output as the runner reads them.
func TestHasUnansweredToolCallReadsCommittedResponses(t *testing.T) {
	call := `{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}`
	answer := `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	tests := []struct {
		name    string
		outputs []string
		want    bool
	}{
		{"committed call without an answer", []string{call}, true},
		{"committed call answered in a later response", []string{call, answer}, false},
		{"committed call answered before a later unanswered call", []string{call, answer, `{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"}`}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			journal, err := NewJournal(filepath.Join(dir, "messages.jsonl"), DurabilityProcess)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewTurnStore(journal, filepath.Join(dir, "staging"), TurnStoreOptions{MaxItemBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			for index, item := range test.outputs {
				turn, err := store.BeginTurn(fmt.Sprintf("turn-%d", index))
				if err != nil {
					t.Fatal(err)
				}
				if err := turn.StageOutputItem(outputItemEvent(0, item)); err != nil {
					t.Fatal(err)
				}
				completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[%s]}}`, index, item))
				if _, err := turn.Commit(completed); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if got := HasUnansweredToolCall(data); got != test.want {
				t.Fatalf("HasUnansweredToolCall = %v, want %v for journal %s", got, test.want, data)
			}
		})
	}
}
