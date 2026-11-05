package session

import (
	"context"
	"encoding/json"
	"errors"
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
