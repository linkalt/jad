package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
)

// scriptedStreamer answers call n with errs[n] when it is set, or with rounds[n].
type scriptedStreamer struct {
	rounds   [][]responses.Event
	errs     []error
	requests []string
}

func (s *scriptedStreamer) Stream(_ context.Context, request json.RawMessage, handle responses.EventHandler) error {
	index := len(s.requests)
	s.requests = append(s.requests, string(request))
	if index < len(s.errs) && s.errs[index] != nil {
		return s.errs[index]
	}
	if index >= len(s.rounds) {
		return errors.New("unexpected provider request")
	}
	for _, event := range s.rounds[index] {
		if err := handle(event); err != nil {
			if errors.Is(err, errTerminalResponseCommitted) {
				return nil
			}
			return err
		}
	}
	return nil
}

var errOverflow = errors.New(`400 Bad Request {"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`)

type overflowCall struct {
	attempt   int
	compacted bool
}

func overflowCompaction(enabled bool, attempts int, calls *[]overflowCall) *AutomaticCompactionSettings {
	return &AutomaticCompactionSettings{
		Enabled: enabled, TriggerTokens: 1_000_000, TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append",
		Overflow: &OverflowSettings{
			MaxAttempts: attempts, Patterns: []string{"context_length_exceeded", "prompt is too long"},
			OnCompacted: func(attempt int, compacted bool, _ error) {
				*calls = append(*calls, overflowCall{attempt, compacted})
			},
		},
	}
}

func TestOverflowCompactsAndSendsTheRoundAgain(t *testing.T) {
	store, journal, _ := newAutomaticCompactionStore(t, "")
	appendOlderCompactionTurn(t, store, journal)
	streamer := &scriptedStreamer{errs: []error{errOverflow}, rounds: [][]responses.Event{nil, finalAnswerRound("recovered", 10)}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []overflowCall
	refreshed := 0
	_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		AutomaticCompaction: overflowCompaction(true, 1, &calls),
		RefreshStateContext: func(context.Context) ([]StateContextPrompt, error) {
			refreshed++
			return []StateContextPrompt{{Role: "developer", Text: "STATE AFTER OVERFLOW"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want the refused one and the retry", len(streamer.requests))
	}
	if !strings.Contains(streamer.requests[0], "older context") || strings.Contains(streamer.requests[1], "older context") {
		t.Fatalf("the retry still sent the dropped turn: %s", streamer.requests[1])
	}
	if !strings.Contains(streamer.requests[1], "new request") || !strings.Contains(streamer.requests[1], "STATE AFTER OVERFLOW") {
		t.Fatalf("retry request = %s", streamer.requests[1])
	}
	if len(calls) != 1 || calls[0] != (overflowCall{1, true}) || refreshed != 1 {
		t.Fatalf("overflow calls = %+v, refreshed = %d", calls, refreshed)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	if !strings.Contains(all, "recovered") {
		t.Fatalf("the retried answer is not in the history: %s", all)
	}
}

func TestOverflowReturnsTheProviderErrorWhenItCannotHelp(t *testing.T) {
	tests := []struct {
		name      string
		enabled   bool
		older     bool
		attempts  int
		errs      []error
		wantCalls []overflowCall
		requests  int
	}{
		{name: "another error", enabled: true, older: true, errs: []error{errors.New("500 server error")}, attempts: 1, requests: 1},
		{name: "automatic compaction off", enabled: false, older: true, errs: []error{errOverflow}, attempts: 1, requests: 1},
		{name: "nothing to drop", enabled: true, older: false, errs: []error{errOverflow}, attempts: 1, wantCalls: []overflowCall{{1, false}}, requests: 1},
		{name: "attempts used up", enabled: true, older: true, errs: []error{errOverflow, errOverflow}, attempts: 1, wantCalls: []overflowCall{{1, true}}, requests: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, journal, _ := newAutomaticCompactionStore(t, "")
			if test.older {
				appendOlderCompactionTurn(t, store, journal)
			}
			streamer := &scriptedStreamer{errs: test.errs}
			runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			var calls []overflowCall
			_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{AutomaticCompaction: overflowCompaction(test.enabled, test.attempts, &calls)})
			if err == nil || !strings.Contains(err.Error(), test.errs[len(test.errs)-1].Error()) {
				t.Fatalf("Run() error = %v, want the provider's", err)
			}
			if len(streamer.requests) != test.requests || len(calls) != len(test.wantCalls) {
				t.Fatalf("requests = %d, overflow calls = %+v", len(streamer.requests), calls)
			}
			for index := range calls {
				if calls[index] != test.wantCalls[index] {
					t.Fatalf("overflow calls = %+v, want %+v", calls, test.wantCalls)
				}
			}
		})
	}
}

func TestIsContextOverflowMatchesWithoutCase(t *testing.T) {
	patterns := []string{"Prompt is too long", " context_length_exceeded "}
	for _, text := range []string{"anthropic: prompt is too long: 210000 tokens > 200000", "code CONTEXT_LENGTH_EXCEEDED"} {
		if !IsContextOverflow(errors.New(text), patterns) {
			t.Errorf("%q not matched", text)
		}
	}
	if IsContextOverflow(errors.New("rate limited"), patterns) || IsContextOverflow(nil, patterns) || IsContextOverflow(errors.New("x"), []string{"  "}) {
		t.Error("a non-overflow error matched")
	}
}

// An overflow in the middle of a tool loop drops the older turns but keeps the run's own
// calls and results, so the retry carries on from them (it used to restart from the user
// message with every call of the run gone).
func TestOverflowKeepsTheRunsToolWork(t *testing.T) {
	store, journal, _ := newAutomaticCompactionStore(t, "")
	appendOlderCompactionTurn(t, store, journal)
	streamer := &scriptedStreamer{
		errs:   []error{nil, errOverflow},
		rounds: [][]responses.Event{tokenRound(0, 1, ""), nil, finalAnswerRound("recovered", 10)},
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []overflowCall
	if _, err := runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		ToolCatalog: newLookupCatalog(t), MaxToolRounds: 8,
		AutomaticCompaction: overflowCompaction(true, 1, &calls),
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 3 || len(calls) != 1 || calls[0] != (overflowCall{1, true}) {
		t.Fatalf("requests = %d, overflow calls = %+v", len(streamer.requests), calls)
	}
	retry := streamer.requests[2]
	if strings.Contains(retry, "older context") || !strings.Contains(retry, "new request") ||
		!strings.Contains(retry, `"function_call"`) || !strings.Contains(retry, `"function_call_output"`) {
		t.Fatalf("the retry lost the run's tool work or kept the older turn: %s", retry)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	for _, want := range []string{"new request", `"call-0-0"`, `"function_call_output"`, "recovered"} {
		if !strings.Contains(all, want) {
			t.Fatalf("history is missing %s: %s", want, all)
		}
	}
}

// When the run's own turn is all there is, an overflow compacts nothing and leaves
// messages.jsonl as it was.
func TestOverflowLeavesTheRunsTurnAloneWhenNothingElseCanGo(t *testing.T) {
	store, _, _ := newAutomaticCompactionStore(t, "")
	streamer := &scriptedStreamer{errs: []error{nil, errOverflow}, rounds: [][]responses.Event{tokenRound(0, 1, "")}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []overflowCall
	settings := overflowCompaction(true, 1, &calls)
	settings.KeepTurns = 0
	_, err = runner.Run(context.Background(), json.RawMessage(`"new request"`), RunOptions{
		ToolCatalog: newLookupCatalog(t), MaxToolRounds: 8, AutomaticCompaction: settings,
	})
	if err == nil || !strings.Contains(err.Error(), errOverflow.Error()) {
		t.Fatalf("Run() error = %v, want the provider's", err)
	}
	if len(calls) != 1 || calls[0] != (overflowCall{1, false}) {
		t.Fatalf("overflow calls = %+v", calls)
	}
	history, err := store.ReadInputItems()
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, item := range history {
		all += string(item)
	}
	if !strings.Contains(all, "new request") || !strings.Contains(all, `"function_call_output"`) {
		t.Fatalf("the run's turn was rewritten: %s", all)
	}
}

// A failed stream is an overflow only by the provider's error code and message, not by
// the instructions or tools its echoed response carries.
func TestIsContextOverflowReadsOnlyTheProvidersErrorOfAFailedStream(t *testing.T) {
	patterns := []string{"context_length_exceeded", "context window", "too many tokens"}
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"echoed instructions", `{"type":"response.failed","response":{"instructions":"Mind your context window.","tools":[{"description":"too many tokens"}],"error":{"code":"server_error","message":"The server had an error"}}}`, false},
		{"response error", `{"type":"response.failed","response":{"instructions":"x","error":{"code":"context_length_exceeded","message":"Too long."}}}`, true},
		{"response error message", `{"type":"response.failed","response":{"error":{"code":"invalid_prompt","message":"Your input exceeds the context window of this model."}}}`, true},
		{"error event", `{"type":"error","code":"context_length_exceeded","message":"Too long.","param":null}`, true},
		{"top-level error object", `{"type":"error","error":{"type":"invalid_request_error","message":"too many tokens in the prompt"}}`, true},
		{"top-level error string", `{"type":"error","error":"context_length_exceeded"}`, true},
		{"error event of another kind", `{"type":"error","code":"rate_limit_exceeded","message":"Slow down."}`, false},
		{"no error at all", `{"type":"response.failed","response":{"instructions":"context window"}}`, false},
	}
	for _, test := range tests {
		eventType := "response.failed"
		if strings.Contains(test.raw, `"type":"error"`) {
			eventType = "error"
		}
		err := terminalFailureError([]byte(test.raw), eventType)
		if got := IsContextOverflow(fmt.Errorf("round: %w", err), patterns); got != test.want {
			t.Errorf("%s: overflow = %v, want %v (detail %q)", test.name, got, test.want, err.(*ResponseFailedError).Detail)
		}
		if !strings.HasPrefix(err.Error(), "Responses stream terminated without response.completed: ") {
			t.Errorf("%s: error text changed: %s", test.name, err)
		}
	}
}
