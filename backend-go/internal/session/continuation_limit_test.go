package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/transcript"
)

// A round a stop hook adds is not started once max_run_tokens is reached: the hook is not
// even run, so its side effects do not happen for a round that could not start, and the
// run ends as it would without a hook (its final answer stands; no cap error).
func TestStopHookRoundIsNotStartedOverTheTokenCap(t *testing.T) {
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{tokenRound(0, 0, seventyTokens), tokenRound(1, 0, seventyTokens)}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	hookRuns := 0
	var skipped []error
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		MaxRunTokens:     50,
		StopMaxContinues: 3,
		OnStop:           func(context.Context, string, string, int) (string, error) { hookRuns++; return "keep going", nil },
		OnStopSkipped:    func(_ context.Context, _ string, limit error) { skipped = append(skipped, limit) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want the run to end as without a stop hook", err)
	}
	if hookRuns != 0 {
		t.Fatalf("the stop hook ran %d times with the cap reached, want 0", hookRuns)
	}
	var limit *RunTokenLimitError
	if len(skipped) != 1 || !errors.As(skipped[0], &limit) {
		t.Fatalf("OnStopSkipped got %v, want the run token cap once", skipped)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(streamer.requests))
	}
	all := ""
	_ = journal.ReadLines(func(line []byte) error { all += string(line); return nil })
	if strings.Contains(all, "keep going") {
		t.Fatalf("the stop hook's message was stored: %s", all)
	}
}

// A budget ledger that cannot be read when the stop hook would be asked fails the run, as
// it does before any round; only a reached cap skips the hook. The round reports no usage,
// so the budget is not written (and read) between the start and the end of the run.
func TestStopHookWithAnUnreadableBudgetFailsTheRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	streamer := &pushingStreamer{scriptedStreamer: scriptedStreamer{rounds: [][]responses.Event{tokenRound(0, 0, `{"input_tokens":0,"output_tokens":0}`)}}}
	streamer.before = func(int) {
		if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
			t.Error(err)
		}
	}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	hookRuns, skips := 0, 0
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		Budget:           &Budget{Ledger: NewBudgetLedger(path), Limits: BudgetLimits{MaxSessionTokens: 1000}},
		StopMaxContinues: 1,
		OnStop:           func(context.Context, string, string, int) (string, error) { hookRuns++; return "", nil },
		OnStopSkipped:    func(context.Context, string, error) { skips++ },
	})
	if err == nil || !strings.Contains(err.Error(), "budget ledger") {
		t.Fatalf("Run() error = %v, want the budget ledger's read error", err)
	}
	if hookRuns != 0 || skips != 0 {
		t.Fatalf("hook runs = %d, skips = %d, want neither", hookRuns, skips)
	}
}

// A steering message waiting when the budget is spent does not get a round; it is
// reported dropped.
func TestSteerRoundIsNotStartedOverTheBudget(t *testing.T) {
	ledger := &Budget{Ledger: NewBudgetLedger(filepath.Join(t.TempDir(), "budget.json")), Limits: BudgetLimits{MaxSessionTokens: 50}}
	queue := NewSteerQueue(4)
	streamer := &pushingStreamer{scriptedStreamer: scriptedStreamer{rounds: [][]responses.Event{tokenRound(0, 0, seventyTokens), tokenRound(1, 0, seventyTokens)}}}
	streamer.before = func(index int) {
		if index == 0 {
			if _, err := queue.Push(json.RawMessage(`"one more thing"`)); err != nil {
				t.Errorf("Push() error = %v", err)
			}
		}
	}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var dropped []json.RawMessage
	_, err = runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		Budget:         ledger,
		Steer:          queue,
		OnSteerDropped: func(inputs []json.RawMessage) { dropped = append(dropped, inputs...) },
	})
	var limit *BudgetLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Run() error = %v, want the budget", err)
	}
	if len(streamer.requests) != 1 || len(dropped) != 1 {
		t.Fatalf("provider requests = %d, dropped = %q", len(streamer.requests), dropped)
	}
}

// A caller that stored the input itself checked the budget before; the run does not
// check it again and leave the stored message unanswered.
func TestRunWithPersistedInputIsNotRefusedByTheBudgetAgain(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "budget.json")
	ledger := &Budget{Ledger: NewBudgetLedger(ledgerPath), Limits: BudgetLimits{MaxSessionTokens: 50}}
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{tokenRound(0, 0, seventyTokens), tokenRound(1, 0, seventyTokens)}}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	// The first run spends the budget.
	if _, err := runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{Budget: ledger}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	persisted, err := PersistInputWithImages(store, json.RawMessage(`"stored by the caller"`), func() (int64, error) { return 2, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"stored by the caller"`), RunOptions{Budget: ledger, PersistedInput: persisted}); err != nil {
		t.Fatalf("Run() with a stored input = %v, want it answered", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(streamer.requests))
	}
}

// A declined draft ends the round it happened in; a steering message's round after it is
// an ordinary round, so the stop hooks run when it ends.
func TestStopHookRunsAfterASteeredRoundThatFollowsADecline(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition:           tools.Definition{ID: "native.request_draft", Source: "native", Name: "request_draft", InputSchema: schema},
		Validator:            validator,
		RequiresUserDecision: true,
		Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			reason := "not now"
			answer := tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionAnswered, Reason: &reason}
			if err := execution.OnInteractionResolved(tools.InteractionIdentity{Tool: "request_draft", ToolCallID: execution.CallID}, answer, json.RawMessage(`{}`)); err != nil {
				return "", err
			}
			return `{"decision":"decline"}`, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item := `{"type":"function_call","call_id":"call-draft","name":"request_draft","arguments":"{}"}`
	draftRound := []responses.Event{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-draft","status":"completed","output":[]}}`)},
	}
	queue := NewSteerQueue(4)
	streamer := &pushingStreamer{scriptedStreamer: scriptedStreamer{rounds: [][]responses.Event{draftRound, finalAnswerRound("done", 1)}}}
	streamer.before = func(index int) {
		if index == 0 {
			if _, err := queue.Push(json.RawMessage(`"do it differently"`)); err != nil {
				t.Errorf("Push() error = %v", err)
			}
		}
	}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	stops := 0
	if _, err := runner.Run(context.Background(), json.RawMessage(`"plan it"`), RunOptions{
		ToolCatalog: catalog, MaxToolRounds: 3, Steer: queue, StopMaxContinues: 1,
		OnStop: func(context.Context, string, string, int) (string, error) { stops++; return "", nil },
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(streamer.requests) != 2 || stops != 1 {
		t.Fatalf("provider requests = %d, stop hook calls = %d; want 2 and 1", len(streamer.requests), stops)
	}
}
