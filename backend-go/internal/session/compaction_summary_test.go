package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

func summaryTestSettings(onFailure string) *CompactionSummarySettings {
	return &CompactionSummarySettings{
		Prompt: "SUMMARIZE PROMPT", Header: "SUMMARY HEADER:", Role: "developer",
		MaxInputBytes: 100000, MaxItemBytes: 4000, MaxOutputTokens: 77, TimeoutMS: 10000, OnFailure: onFailure,
	}
}

func finalAnswerRound(text string, inputTokens int) []responses.Event {
	item := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
	return []responses.Event{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-` + text + `","status":"completed","usage":{"input_tokens":` + itoa(inputTokens) + `},"output":[` + item + `]}}`)},
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAutomaticCompactionKeepsTheModelsSummaryOfTheDroppedTurns(t *testing.T) {
	store, journal, _ := newAutomaticCompactionStore(t, "")
	appendOlderCompactionTurn(t, store, journal)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		finalAnswerRound("answer", 100),
		finalAnswerRound("SUMMARY-TEXT", 10),
	}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test", PromptRole: "developer", PromptText: "session prompt", Request: json.RawMessage(`{"tool_choice":"auto","max_output_tokens":9000}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		AutomaticCompaction: &AutomaticCompactionSettings{
			Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
			Summary: summaryTestSettings(CompactionSummaryOnFailureCompact),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want the run's round and the summary", len(streamer.requests))
	}
	summaryRequest := string(streamer.requests[1])
	for _, want := range []string{"SUMMARIZE PROMPT", "older context", `"max_output_tokens":77`} {
		if !strings.Contains(summaryRequest, want) {
			t.Errorf("summary request misses %q: %s", want, summaryRequest)
		}
	}
	for _, unwanted := range []string{"session prompt", "tool_choice", `"tools"`, "new request"} {
		if strings.Contains(summaryRequest, unwanted) {
			t.Errorf("summary request carries %q: %s", unwanted, summaryRequest)
		}
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("history is empty after compaction")
	}
	first := string(history[0])
	if !strings.Contains(first, `"role":"developer"`) || !strings.Contains(first, `SUMMARY HEADER:\n\nSUMMARY-TEXT`) {
		t.Fatalf("first history item = %s, want the stored summary", first)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	if strings.Contains(all, "older context") || !strings.Contains(all, "new request") {
		t.Fatalf("history after compaction = %s", all)
	}
}

func TestCompactionSummaryFailureCompactsOrSkipsByOnFailure(t *testing.T) {
	for _, onFailure := range []string{CompactionSummaryOnFailureCompact, CompactionSummaryOnFailureSkip} {
		t.Run(onFailure, func(t *testing.T) {
			store, journal, _ := newAutomaticCompactionStore(t, "")
			appendOlderCompactionTurn(t, store, journal)
			failed := responses.Event{Type: "response.failed", Raw: json.RawMessage(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"}}}`)}
			streamer := &multiRoundStreamer{rounds: [][]responses.Event{finalAnswerRound("answer", 100), {failed}}}
			runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			var reported []error
			_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
				AutomaticCompaction: &AutomaticCompactionSettings{
					Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
					Summary:        summaryTestSettings(onFailure),
					OnSummaryError: func(err error) { reported = append(reported, err) },
				},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(reported) != 1 {
				t.Fatalf("reported summary errors = %v, want one", reported)
			}
			history, err := store.ReadInputItems()
			if err != nil {
				t.Fatal(err)
			}
			all := ""
			for _, item := range history {
				all += string(item)
			}
			if strings.Contains(all, "SUMMARY HEADER") {
				t.Fatalf("a failed summary was stored: %s", all)
			}
			keptOlder := strings.Contains(all, "older context")
			if onFailure == CompactionSummaryOnFailureSkip && !keptOlder {
				t.Fatalf("on_failure skip compacted anyway: %s", all)
			}
			if onFailure == CompactionSummaryOnFailureCompact && keptOlder {
				t.Fatalf("on_failure compact did not compact: %s", all)
			}
		})
	}
}

func TestCompactionSummaryIsNotRequestedWhenNothingIsDropped(t *testing.T) {
	store, _, _ := newAutomaticCompactionStore(t, "")
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{finalAnswerRound("answer", 100)}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"only request"`), RunOptions{
		AutomaticCompaction: &AutomaticCompactionSettings{
			Enabled: true, TriggerTokens: 100, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
			Summary: summaryTestSettings(CompactionSummaryOnFailureCompact),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("provider requests = %d, want no summary request for a single turn", len(streamer.requests))
	}
}

func TestCompactHistoryRefusesASummaryOfOtherTurns(t *testing.T) {
	store, journal, _ := newAutomaticCompactionStore(t, "")
	appendOlderCompactionTurn(t, store, journal)
	if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err := store.CompactHistory(transcript.CompactionOptions{
		TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
		Summarize: func([][]byte) (json.RawMessage, error) {
			// A turn lands while the summary is written.
			if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"third"}]}`)); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"s"}]}`), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed while the compaction summary was written") {
		t.Fatalf("CompactHistory() error = %v, want the changed-transcript refusal", err)
	}
	_, err = store.CompactHistory(transcript.CompactionOptions{
		TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
		Summarize: func([][]byte) (json.RawMessage, error) { return nil, transcript.ErrCompactionSkipped },
	})
	if err != nil {
		t.Fatalf("skipped compaction error = %v", err)
	}
	history, _ := store.ReadInputItems()
	if len(history) < 3 {
		t.Fatalf("a skipped compaction changed the history: %d items", len(history))
	}
}

func TestRenderItemsForSummaryKeepsTheEarlierSummaryAndTheNewestItems(t *testing.T) {
	items := [][]byte{
		[]byte(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"HDR\n\nold facts"}]}`),
		[]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"first ` + strings.Repeat("x", 300) + `"}]}`),
		[]byte(`{"type":"reasoning","summary":[],"encrypted_content":"secret"}`),
		[]byte(`{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"ls\"}"}`),
		[]byte(`{"type":"function_call_output","call_id":"c1","output":"a.go\nb.go"}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	all := RenderItemsForSummary(items, "HDR", 100, 100000)
	for _, want := range []string{"Earlier summary:\nold facts", "tool call bash (c1)", "tool result (c1):\na.go", "assistant:\ndone", "bytes cut]"} {
		if !strings.Contains(all, want) {
			t.Errorf("rendered text misses %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "secret") {
		t.Errorf("reasoning was rendered:\n%s", all)
	}
	if !strings.HasPrefix(all, "Earlier summary:") {
		t.Errorf("the earlier summary is not first:\n%s", all)
	}
	small := RenderItemsForSummary(items, "HDR", 100, 120)
	if !strings.Contains(small, "older items left out") || !strings.Contains(small, "assistant:\ndone") || strings.Contains(small, "first x") {
		t.Errorf("budgeted text:\n%s", small)
	}
	if !strings.HasPrefix(small, "Earlier summary:") {
		t.Errorf("budgeted text lost the earlier summary:\n%s", small)
	}
}

func TestCompactionSummarizerReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summarize := NewCompactionSummarizer(ctx, &fakeStreamer{err: context.Canceled}, ModelSettings{Model: "m"}, *summaryTestSettings(CompactionSummaryOnFailureCompact), nil)
	_, err := summarize([][]byte{[]byte(`{"type":"message","role":"user","content":"hi"}`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("summarize() error = %v, want the cancellation", err)
	}
}
