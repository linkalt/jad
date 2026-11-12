package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/transcript"
)

// recordingCatalog has one tool, lookup, that records the call id of every execution.
func recordingCatalog(t *testing.T, executed *[]string) *tools.Catalog {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: "native.lookup", Source: "native", Name: "lookup", Description: "Look up one query.", InputSchema: schema},
		Validator:  validator,
		Execute: func(_ context.Context, args map[string]any, execution tools.ExecutionContext) (string, error) {
			*executed = append(*executed, execution.CallID)
			return fmt.Sprintf("%s:%s", execution.CallID, args["query"]), nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func lookupCall(id, callID, query string) string {
	arguments, _ := json.Marshal(map[string]string{"query": query})
	argumentText, _ := json.Marshal(string(arguments))
	return fmt.Sprintf(`{"type":"function_call","id":%q,"call_id":%q,"name":"lookup","arguments":%s,"status":"completed"}`, id, callID, argumentText)
}

func doneAt(index int, item string) responses.Event {
	return responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"sequence_number":9,"item":%s}`, index, item))}
}

// providerRound is a stream the way the observed provider sent it: lifecycle events,
// output_item.added and argument events that carry an index, the done events in the
// given order, response.metadata, then response.completed with the given output.
func providerRound(done []responses.Event, output ...string) []responses.Event {
	events := []responses.Event{
		{Type: "response.created", Raw: json.RawMessage(`{"type":"response.created","response":{"id":"resp_gap","status":"in_progress"}}`)},
		{Type: "response.in_progress", Raw: json.RawMessage(`{"type":"response.in_progress","response":{"id":"resp_gap","status":"in_progress"}}`)},
		{Type: "response.function_call_arguments.delta", Raw: json.RawMessage(`{"type":"response.function_call_arguments.delta","item_id":"fc_x","output_index":1,"delta":"{"}`)},
	}
	events = append(events, done...)
	events = append(events,
		responses.Event{Type: "response.metadata", Raw: json.RawMessage(`{"type":"response.metadata","metadata":{}}`)},
		responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp_gap","status":"completed","usage":{"input_tokens":3,"output_tokens":2},"output":[` + strings.Join(output, ",") + `]}}`)},
	)
	return events
}

type reconcileRun struct {
	executed []string
	requests []json.RawMessage
	records  []string
	err      error
}

func runReconcile(t *testing.T, first []responses.Event) reconcileRun {
	t.Helper()
	var run reconcileRun
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{first, finalAnswerRound("done", 1)}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, run.err = runner.Run(context.Background(), json.RawMessage(`"find"`), RunOptions{
		TurnID:        "run-reconcile",
		ToolCatalog:   recordingCatalog(t, &run.executed),
		MaxToolRounds: 2,
	})
	run.requests = streamer.requests
	if err := journal.ReadLines(func(line []byte) error {
		run.records = append(run.records, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

// callOrder lists the call ids of the function_call items of a transcript record, in
// the order the record holds them.
func callOrder(t *testing.T, record string) []string {
	t.Helper()
	var envelope struct {
		Response struct {
			Output []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(record), &envelope); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range envelope.Response.Output {
		if item.Type == "function_call" {
			ids = append(ids, item.CallID)
		}
	}
	return ids
}

// toolOutputOrder lists the call ids of the function_call_output records, in order.
func toolOutputOrder(records []string) []string {
	var ids []string
	for _, record := range records {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal([]byte(record), &item) == nil && item.Type == "function_call_output" {
			ids = append(ids, item.CallID)
		}
	}
	return ids
}

func TestRunCommitsAndExecutesCallsWhenTheProviderIndexesDoNotMatchTheOutput(t *testing.T) {
	a, b, c := lookupCall("fc_a", "call_a", "alpha"), lookupCall("fc_b", "call_b", "beta"), lookupCall("fc_c", "call_c", "gamma")
	cases := []struct {
		name         string
		round        []responses.Event
		wantExecuted string
	}{
		{"observed shape: the only call has index 1", providerRound([]responses.Event{doneAt(1, a)}, a), "call_a"},
		{"the only call has index 2", providerRound([]responses.Event{doneAt(2, a)}, a), "call_a"},
		{"two calls at 1 and 2", providerRound([]responses.Event{doneAt(1, a), doneAt(2, b)}, a, b), "call_a,call_b"},
		{"done events arrive in the reverse of the output", providerRound([]responses.Event{doneAt(2, b), doneAt(1, a)}, a, b), "call_a,call_b"},
		{"output order differs from index order", providerRound([]responses.Event{doneAt(0, a), doneAt(1, b), doneAt(2, c)}, c, a, b), "call_c,call_a,call_b"},
		{"gaps, reorder and an arrival order unlike both", providerRound([]responses.Event{doneAt(8, c), doneAt(3, a), doneAt(5, b)}, b, c, a), "call_b,call_c,call_a"},
		{"dense and in order", providerRound([]responses.Event{doneAt(0, a), doneAt(1, b)}, a, b), "call_a,call_b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runReconcile(t, tc.round)
			if run.err != nil {
				t.Fatalf("Run() error = %v", run.err)
			}
			want := tc.wantExecuted
			if got := strings.Join(run.executed, ","); got != want {
				t.Fatalf("executed %s, want %s", got, want)
			}
			// The first response record is the second record of the transcript, after the input.
			if got := strings.Join(callOrder(t, run.records[1]), ","); got != want {
				t.Fatalf("transcript holds calls %s, want %s", got, want)
			}
			if got := strings.Join(toolOutputOrder(run.records), ","); got != want {
				t.Fatalf("tool outputs for %s, want %s", got, want)
			}
			if len(run.requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(run.requests))
			}
		})
	}
}

func TestRunRefusesAProviderStreamItCannotMatchUnambiguously(t *testing.T) {
	a, b := lookupCall("fc_a", "call_a", "alpha"), lookupCall("fc_b", "call_b", "beta")
	otherArguments := lookupCall("fc_a", "call_a", "changed")
	noID := func(callID string) string {
		return fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{\"query\":\"q\"}"}`, callID)
	}
	cases := []struct {
		name  string
		round []responses.Event
		want  string
	}{
		{"completed call that was never staged", providerRound([]responses.Event{doneAt(1, a)}, b), "has no staged item"},
		{"completed call with another call's id", providerRound([]responses.Event{doneAt(1, a), doneAt(2, b)}, a, lookupCall("fc_a", "call_b", "beta")), "has no staged item"},
		{"call whose arguments differ between the done event and the output", providerRound([]responses.Event{doneAt(1, a)}, otherArguments), "differs from staged output_index 1 in arguments"},
		{"duplicate ids on the wire", providerRound([]responses.Event{doneAt(1, noID("call_a")), doneAt(2, noID("call_a"))}, noID("call_a"), noID("call_a")), "ambiguous"},
		{"output holds a call the stream never finished", providerRound([]responses.Event{doneAt(1, a)}, a, b), "does not match completed output count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runReconcile(t, tc.round)
			if run.err == nil || !strings.Contains(run.err.Error(), tc.want) {
				t.Fatalf("Run() error = %v, want it to contain %q", run.err, tc.want)
			}
			if len(run.executed) != 0 {
				t.Fatalf("a refused response executed %v", run.executed)
			}
			if len(run.requests) != 1 {
				t.Fatalf("provider requests = %d, want 1", len(run.requests))
			}
			for _, record := range run.records {
				if strings.Contains(record, `"function_call"`) {
					t.Fatalf("a refused response reached the transcript: %s", record)
				}
			}
		})
	}
}

func TestRunWithoutProviderIDsKeepsPositionalPairing(t *testing.T) {
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking"}]}`
	call := `{"type":"function_call","call_id":"call_p","name":"lookup","arguments":"{\"query\":\"q\"}"}`
	run := runReconcile(t, providerRound([]responses.Event{doneAt(0, message), doneAt(1, call)}, message, call))
	if run.err != nil {
		t.Fatalf("Run() error = %v", run.err)
	}
	if got := strings.Join(run.executed, ","); got != "call_p" {
		t.Fatalf("executed %s", got)
	}
	if !strings.Contains(run.records[1], `"looking"`) {
		t.Fatalf("the message was lost: %s", run.records[1])
	}
}

func TestRunRebuildsOmittedOutputFromStagedItemsWithAGap(t *testing.T) {
	a := lookupCall("fc_a", "call_a", "alpha")
	run := runReconcile(t, providerRound([]responses.Event{doneAt(1, a)}))
	if run.err != nil {
		t.Fatalf("Run() error = %v", run.err)
	}
	if got := strings.Join(run.executed, ","); got != "call_a" {
		t.Fatalf("executed %s", got)
	}
	if got := strings.Join(callOrder(t, run.records[1]), ","); got != "call_a" {
		t.Fatalf("transcript holds calls %s", got)
	}
}
