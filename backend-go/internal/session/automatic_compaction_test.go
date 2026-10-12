package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func TestRunnerAutomaticallyCompactsBeforeToolRoundAndRefreshesStateContext(t *testing.T) {
	store, journal, root := newAutomaticCompactionStore(t, "")
	appendOlderCompactionTurn(t, store, journal)

	callItem := `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"query\":\"latest\"}"}`
	firstItem := responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + callItem + `}`)}
	firstCompleted := responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-tool","status":"completed","usage":{"input_tokens":100},"output":[]}}`)}
	finalItem := responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)}
	second := responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[]}}`)}
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{{firstItem, firstCompleted}, {finalItem, second}}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	refreshCount := 0
	refreshSawToolOutput := false
	result, err := runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		TurnID:        "auto-compact",
		ToolCatalog:   newLookupCatalog(t),
		MaxToolRounds: 1,
		AutomaticCompaction: &AutomaticCompactionSettings{
			Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1,
			BackupMode: "append",
		},
		RefreshStateContext: func(context.Context) ([]StateContextPrompt, error) {
			refreshCount++
			history, err := store.ReadInputItems()
			if err != nil {
				return nil, err
			}
			for _, item := range history {
				if strings.Contains(string(item), `"type":"function_call_output"`) {
					refreshSawToolOutput = true
				}
			}
			return []StateContextPrompt{{Role: "developer", Text: "fresh state after compaction"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TurnID != "auto-compact" || len(streamer.requests) != 2 {
		t.Fatalf("turn=%q provider requests=%d", result.TurnID, len(streamer.requests))
	}
	if refreshCount != 1 || !refreshSawToolOutput {
		t.Fatalf("statecontext refresh count=%d, saw persisted tool output=%t", refreshCount, refreshSawToolOutput)
	}
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(streamer.requests[1], &request); err != nil {
		t.Fatal(err)
	}
	requestText := string(streamer.requests[1])
	for _, expected := range []string{"new request", "function_call", "function_call_output", "fresh state after compaction"} {
		if !strings.Contains(requestText, expected) {
			t.Errorf("second provider request missing %q: %s", expected, requestText)
		}
	}
	if strings.Contains(requestText, "older context") {
		t.Errorf("second provider request retained compacted history: %s", requestText)
	}
	if len(request.Input) < 4 {
		t.Fatalf("second provider input has %d items, want user, call, tool output, and refreshed context", len(request.Input))
	}
	if _, err := os.Stat(filepath.Join(root, "messages.backup.jsonl")); err != nil {
		t.Fatalf("automatic compaction backup: %v", err)
	}
}

func TestRunnerSkipsAutomaticCompactionWhenDisabledBelowThresholdOrIncomplete(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		threshold   int64
		status      string
		inputTokens string
	}{
		{name: "disabled", enabled: false, threshold: 1, status: "completed", inputTokens: "100"},
		{name: "below threshold", enabled: true, threshold: 101, status: "completed", inputTokens: "100"},
		{name: "incomplete response", enabled: true, threshold: 1, status: "incomplete", inputTokens: "100"},
		{name: "non-numeric usage", enabled: true, threshold: 1, status: "completed", inputTokens: `"100"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, journal, root := newAutomaticCompactionStore(t, "")
			appendOlderCompactionTurn(t, store, journal)
			eventType := "response.completed"
			if test.status == "incomplete" {
				eventType = "response.incomplete"
			}
			event := responses.Event{Type: eventType, Raw: json.RawMessage(fmt.Sprintf(`{"type":%q,"response":{"id":"resp-test","status":%q,"usage":{"input_tokens":%s},"output":[]}}`, eventType, test.status, test.inputTokens))}
			runner, err := NewRunner(store, &fakeStreamer{events: []responses.Event{event}}, ModelSettings{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
				AutomaticCompaction: &AutomaticCompactionSettings{
					Enabled: test.enabled, TriggerTokens: test.threshold, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
				},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			history, err := store.ReadInputItems()
			if err != nil {
				t.Fatal(err)
			}
			if len(history) < 3 || !strings.Contains(string(history[0]), "older context") {
				t.Fatalf("history was unexpectedly compacted: %q", history)
			}
			if _, err := os.Stat(filepath.Join(root, "messages.backup.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("automatic compaction backup exists or stat failed: %v", err)
			}
		})
	}
}

func TestRunnerReturnsAutomaticCompactionFailure(t *testing.T) {
	root := t.TempDir()
	backupDirectory := filepath.Join(root, "backup-is-a-directory")
	if err := os.Mkdir(backupDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, journal, _ := newAutomaticCompactionStore(t, backupDirectory)
	appendOlderCompactionTurn(t, store, journal)
	event := responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-fail","status":"completed","usage":{"input_tokens":100},"output":[]}}`)}
	runner, err := NewRunner(store, &fakeStreamer{events: []responses.Event{event}}, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		AutomaticCompaction: &AutomaticCompactionSettings{Enabled: true, TriggerTokens: 1, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append"},
	})
	if err == nil || !strings.Contains(err.Error(), "automatically compact session transcript") {
		t.Fatalf("Run() error = %v, want automatic compaction failure", err)
	}
}

func newAutomaticCompactionStore(t *testing.T, backupPath string) (*transcript.TurnStore, *transcript.Journal, string) {
	t.Helper()
	root := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(root, "messages.jsonl"), transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, "staging")
	if backupPath == "" {
		backupPath = filepath.Join(root, "messages.backup.jsonl")
	}
	store, err := transcript.NewTurnStore(journal, staging, transcript.TurnStoreOptions{
		MaxItemBytes:        1 << 20,
		ResponseMetadata:    transcript.ResponseMetadataFull,
		MessagesBackupPath:  backupPath,
		CompactionCachePath: filepath.Join(staging, "compaction-cache.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, journal, root
}

func appendOlderCompactionTurn(t *testing.T, store *transcript.TurnStore, journal *transcript.Journal) {
	t.Helper()
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"older context"}]}`)); err != nil {
		t.Fatal(err)
	}
	olderResponse := `{"type":"response.completed","response":{"id":"resp-old","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("old detail ", 30) + `"}]}]}}`
	if err := journal.Append([]byte(olderResponse)); err != nil {
		t.Fatal(err)
	}
}
