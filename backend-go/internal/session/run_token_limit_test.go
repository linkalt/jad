package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/transcript"
)

func TestResponseTokensUsesTotalElseInputPlusOutputElseZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		want  int64
	}{
		{"reported total wins", `{"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":20}}}`, 20},
		{"input plus output", `{"response":{"usage":{"input_tokens":10,"output_tokens":4}}}`, 14},
		{"half a pair is not guessed", `{"response":{"usage":{"input_tokens":10}}}`, 0},
		{"invalid total falls back", `{"response":{"usage":{"total_tokens":-1,"input_tokens":3,"output_tokens":2}}}`, 5},
		{"fractional total", `{"response":{"usage":{"total_tokens":1.5}}}`, 0},
		{"usage is a string", `{"response":{"usage":"lots"}}`, 0},
		{"usage is null", `{"response":{"usage":null}}`, 0},
		{"no usage", `{"response":{}}`, 0},
		{"not json", `nope`, 0},
	} {
		if got := responseTokens(json.RawMessage(tc.event)); got != tc.want {
			t.Errorf("%s: responseTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRunTokenLimitErrorNamesTheCountAndTheLimitOnly(t *testing.T) {
	got := (&RunTokenLimitError{Used: 120, Limit: 100}).Error()
	want := "The run stopped before its next round: it used 120 tokens, which reached the max_run_tokens limit of 100."
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// tokenRound is one scripted provider response: it asks for `calls` lookups (0 makes it a
// final answer) and reports the usage JSON, or none when usage is empty.
func tokenRound(n, calls int, usage string) []responses.Event {
	var events []responses.Event
	var output []string
	for i := 0; i < calls; i++ {
		item := fmt.Sprintf(`{"type":"function_call","call_id":"call-%d-%d","name":"lookup","arguments":"{\"query\":\"q%d\"}"}`, n, i, i)
		output = append(output, item)
		events = append(events, responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":` + fmt.Sprint(i) + `,"item":` + item + `}`)})
	}
	if calls == 0 {
		item := fmt.Sprintf(`{"id":"msg-%d","type":"message","role":"assistant","content":[{"type":"output_text","text":"all done"}]}`, n)
		output = append(output, item)
		events = append(events, responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)})
	}
	usageField := ""
	if usage != "" {
		usageField = `,"usage":` + usage
	}
	completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","status":"completed"%s,"output":[%s]}}`, n, usageField, strings.Join(output, ","))
	return append(events, responses.Event{Type: "response.completed", Raw: json.RawMessage(completed)})
}

const seventyTokens = `{"input_tokens":60,"output_tokens":10}`

func runWithTokenCap(t *testing.T, rounds [][]responses.Event, maxRunTokens int64, maxToolRounds int) (*multiRoundStreamer, []string, error) {
	t.Helper()
	streamer := &multiRoundStreamer{rounds: rounds}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := runner.Run(context.Background(), json.RawMessage(`"go"`), RunOptions{
		ToolCatalog:   newLookupCatalog(t),
		MaxToolRounds: maxToolRounds,
		MaxRunTokens:  maxRunTokens,
	})
	var records []string
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return streamer, records, runErr
}

// outputsOf returns the function_call_output records, in order.
func outputsOf(records []string) []string {
	var outputs []string
	for _, record := range records {
		if strings.Contains(record, `"function_call_output"`) {
			outputs = append(outputs, record)
		}
	}
	return outputs
}

// Each scripted round reports 70 tokens, so after n rounds a run has used 70 * n.

func TestRunStopsBeforeAToolRoundOnceTheCapIsReached(t *testing.T) {
	streamer, records, err := runWithTokenCap(t, [][]responses.Event{
		tokenRound(1, 2, seventyTokens), tokenRound(2, 2, seventyTokens), tokenRound(3, 0, seventyTokens),
	}, 100, 10)
	var limit *RunTokenLimitError
	if !errors.As(err, &limit) || limit.Used != 140 || limit.Limit != 100 {
		t.Fatalf("Run() error = %v, want a RunTokenLimitError for 140 of 100", err)
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (round 1 is 70, round 2 reaches 140, no third)", len(streamer.requests))
	}
	outputs := outputsOf(records)
	if len(outputs) != 4 {
		t.Fatalf("function_call_output records = %d, want one per call (4): %q", len(outputs), outputs)
	}
	for i, output := range outputs {
		if got := strings.Contains(output, "run_token_limit"); got != (i >= 2) {
			t.Errorf("output %d marked run_token_limit = %v, want %v: %s", i, got, i >= 2, output)
		}
	}
}

func TestRunWithACapBelowTheFirstResponseStopsAfterOneRequest(t *testing.T) {
	streamer, _, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 1, seventyTokens), tokenRound(2, 0, seventyTokens)}, 50, 10)
	var limit *RunTokenLimitError
	if !errors.As(err, &limit) || limit.Used != 70 {
		t.Fatalf("Run() error = %v, want used 70", err)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(streamer.requests))
	}
}

func TestUsageExactlyAtTheCapStopsTheRun(t *testing.T) {
	streamer, _, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 1, seventyTokens), tokenRound(2, 0, seventyTokens)}, 70, 10)
	var limit *RunTokenLimitError
	if !errors.As(err, &limit) || len(streamer.requests) != 1 {
		t.Fatalf("error = %v, requests = %d; want a limit error after 1 request (reached, not exceeded)", err, len(streamer.requests))
	}
}

func TestAFinalAnswerCompletesEvenWhenTheCapWasPassed(t *testing.T) {
	streamer, _, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 0, `{"input_tokens":900,"output_tokens":100}`)}, 100, 10)
	if err != nil || len(streamer.requests) != 1 {
		t.Fatalf("error = %v, requests = %d; want a completed run, no round is left to start", err, len(streamer.requests))
	}
}

func TestACapAboveWhatTheRunUsesChangesNothing(t *testing.T) {
	streamer, records, err := runWithTokenCap(t, [][]responses.Event{
		tokenRound(1, 1, seventyTokens), tokenRound(2, 1, seventyTokens), tokenRound(3, 0, seventyTokens),
	}, 100_000, 10)
	if err != nil || len(streamer.requests) != 3 {
		t.Fatalf("error = %v, requests = %d; want 3 requests and no error", err, len(streamer.requests))
	}
	for _, output := range outputsOf(records) {
		if strings.Contains(output, "run_token_limit") {
			t.Errorf("unexpected run_token_limit output: %s", output)
		}
	}
}

func TestNoCapMeansNoLimitHoweverManyTokensAreReported(t *testing.T) {
	huge := `{"input_tokens":5000000,"output_tokens":1}`
	streamer, _, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 1, huge), tokenRound(2, 1, huge), tokenRound(3, 0, huge)}, 0, 10)
	if err != nil || len(streamer.requests) != 3 {
		t.Fatalf("error = %v, requests = %d; want 3 requests and no error", err, len(streamer.requests))
	}
}

func TestResponsesWithoutUsageNeverTripTheCap(t *testing.T) {
	streamer, _, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 1, ""), tokenRound(2, 1, ""), tokenRound(3, 0, "")}, 1, 10)
	if err != nil || len(streamer.requests) != 3 {
		t.Fatalf("error = %v, requests = %d; want 3 requests and no error", err, len(streamer.requests))
	}
}

func TestTheToolRoundLimitKeepsItsOwnErrorWhenItComesFirst(t *testing.T) {
	_, records, err := runWithTokenCap(t, [][]responses.Event{tokenRound(1, 1, seventyTokens)}, 1_000_000, 0)
	var limit *RunTokenLimitError
	if err == nil || errors.As(err, &limit) || !strings.Contains(err.Error(), "exceeded 0 tool rounds") {
		t.Fatalf("Run() error = %v, want the tool-round limit", err)
	}
	if outputs := outputsOf(records); len(outputs) != 1 || !strings.Contains(outputs[0], "tool_round_limit") {
		t.Fatalf("outputs = %q, want one tool_round_limit output", outputs)
	}
}
