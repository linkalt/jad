package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/tools"
	"example.com/agent-harness/backend-go/internal/transcript"
)

type multiRoundStreamer struct {
	rounds   [][]responses.Event
	requests []json.RawMessage
}

func (s *multiRoundStreamer) Stream(_ context.Context, request json.RawMessage, handle responses.EventHandler) error {
	s.requests = append(s.requests, cloneRaw(request))
	if len(s.requests) > len(s.rounds) {
		return fmt.Errorf("unexpected provider round %d", len(s.requests))
	}
	for _, event := range s.rounds[len(s.requests)-1] {
		if err := handle(event); err != nil {
			if errors.Is(err, errTerminalResponseCommitted) {
				return nil
			}
			return err
		}
	}
	return nil
}

func newLookupCatalog(t *testing.T) *tools.Catalog {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1}},"required":["query"],"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: "native.lookup", Source: "native", Name: "lookup", Description: "Look up one query.", InputSchema: schema},
		Validator:  validator,
		Execute: func(_ context.Context, args map[string]any, execution tools.ExecutionContext) (string, error) {
			return fmt.Sprintf("%s:%s", execution.CallID, args["query"]), nil
		},
	}})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func TestRunnerExecutesValidatedToolAndContinuesNextProviderRound(t *testing.T) {
	firstItem := json.RawMessage(`{"type":"function_call","call_id":"call-search","name":"lookup","arguments":"{\"query\":\"hello\"}"}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + string(firstItem) + `}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-tools","status":"completed","output":[]}}`)},
		},
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[]}}`)},
		},
	}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataFull)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), json.RawMessage(`"find hello"`), RunOptions{
		TurnID:        "run-tools",
		ToolCatalog:   newLookupCatalog(t),
		MaxToolRounds: 1,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TurnID != "run-tools" || len(streamer.requests) != 2 {
		t.Fatalf("result turn=%q provider requests=%d", result.TurnID, len(streamer.requests))
	}
	for index, rawRequest := range streamer.requests {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(rawRequest, &request); err != nil {
			t.Fatal(err)
		}
		if len(request.Tools) != 1 {
			t.Fatalf("round %d tools = %d, want 1", index+1, len(request.Tools))
		}
		if index == 1 {
			var toolOutput struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(request.Input[len(request.Input)-1], &toolOutput); err != nil {
				t.Fatal(err)
			}
			if toolOutput.Type != "function_call_output" || toolOutput.CallID != "call-search" || !strings.Contains(toolOutput.Output, "call-search:hello") {
				t.Fatalf("second request last input = %+v", toolOutput)
			}
		}
	}
	var final struct {
		SessionTurn int64 `json:"session_turn"`
		SessionStep int64 `json:"session_step"`
	}
	if err := json.Unmarshal(result.Event, &final); err != nil {
		t.Fatal(err)
	}
	if final.SessionTurn != 1 || final.SessionStep != 2 {
		t.Fatalf("final response session turn/step = %d/%d, want 1/2", final.SessionTurn, final.SessionStep)
	}
	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("transcript records = %d, want input, tool-call response, tool output, final response", len(records))
	}
	if !strings.Contains(string(records[2]), `"type":"function_call_output"`) {
		t.Fatalf("tool output transcript record = %s", records[2])
	}
}

func TestRunnerDefersEveryCoBatchedCallUntilUserDecisionIsResolved(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	otherExecutions := 0
	decisionExecutions := 0
	catalog, err := tools.NewCatalog([]tools.Tool{
		{
			Definition: tools.Definition{ID: "native.lookup", Source: "native", Name: "lookup", InputSchema: schema},
			Validator:  validator,
			Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
				otherExecutions++
				return "unexpected execution", nil
			},
		},
		{
			Definition:           tools.Definition{ID: "native.request_draft", Source: "native", Name: "request_draft", InputSchema: schema},
			Validator:            validator,
			RequiresUserDecision: true,
			Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
				decisionExecutions++
				return `{"decision":"accept"}`, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"call-before","name":"lookup","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call-decision","name":"request_draft","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call-after","name":"lookup","arguments":"{}"}`),
	}
	round, err := executeToolRound(context.Background(), catalog, calls, tools.ExecutionContext{}, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed := round.Completed
	if otherExecutions != 0 || decisionExecutions != 1 || len(completed) != 3 {
		t.Fatalf("other executions=%d decision executions=%d outputs=%d", otherExecutions, decisionExecutions, len(completed))
	}
	for index, callID := range []string{"call-before", "call-decision", "call-after"} {
		var output struct {
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal(completed[index], &output); err != nil {
			t.Fatal(err)
		}
		if output.CallID != callID {
			t.Fatalf("completed output %d call_id=%q, want %q", index, output.CallID, callID)
		}
		if index != 1 && !strings.Contains(output.Output, "user_decision_required") {
			t.Fatalf("deferred output %d = %s", index, output.Output)
		}
	}
}

func TestRunnerStopsAfterDeclinedUserDecisionWithoutAnotherProviderRound(t *testing.T) {
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
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-draft","status":"completed","output":[]}}`)},
	}}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"plan it"`), RunOptions{ToolCatalog: catalog, MaxToolRounds: 3}); err != nil {
		t.Fatalf("Run() error = %v, want a clean stop after the decline", err)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1: a declined draft must not trigger another round", len(streamer.requests))
	}
	var records int
	if err := journal.ReadLines(func([]byte) error { records++; return nil }); err != nil {
		t.Fatal(err)
	}
	if records != 3 {
		t.Fatalf("transcript records = %d, want input, tool call and the decline result", records)
	}
}

func TestRunnerRepairsPendingFunctionCallBeforeAppendingNewInput(t *testing.T) {
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-recovered","status":"completed","output":[]}}`)},
	}}}
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	if err := store.AppendInputItems([]byte(`{"type":"function_call","call_id":"call-interrupted","name":"lookup","arguments":"{}"}`)); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"continue"`), RunOptions{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(streamer.requests[0], &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 3 {
		t.Fatalf("request input items = %d, want function call, repaired output, and new user input", len(request.Input))
	}
	var repaired struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(request.Input[1], &repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Type != "function_call_output" || repaired.CallID != "call-interrupted" || !strings.Contains(repaired.Output, "tool_result_missing") {
		t.Fatalf("repaired function-call output = %+v", repaired)
	}
}

func TestRunnerPersistsToolRoundLimitOutputBeforeFailing(t *testing.T) {
	firstItem := json.RawMessage(`{"type":"function_call","call_id":"call-limit","name":"lookup","arguments":"{\"query\":\"hello\"}"}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + string(firstItem) + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-limit","status":"completed","output":[]}}`)},
	}}}
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), json.RawMessage(`"find hello"`), RunOptions{
		ToolCatalog:   newLookupCatalog(t),
		MaxToolRounds: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeded 0 tool rounds") {
		t.Fatalf("Run() error = %v, want tool-round limit", err)
	}
	if len(streamer.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(streamer.requests))
	}
	var records [][]byte
	if err := journal.ReadLines(func(line []byte) error {
		records = append(records, append([]byte(nil), line...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || !strings.Contains(string(records[2]), "tool_round_limit") {
		t.Fatalf("transcript records = %q, want persisted tool-round-limit output", records)
	}
}

// newSleepCatalog returns a tool whose call sleeps for the number of
// milliseconds in its "ms" argument, so a test controls completion order.
func newSleepCatalog(t *testing.T) (*tools.Catalog, *atomic.Int64) {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"ms":{"type":"integer"}},"required":["ms"],"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	var active, peak atomic.Int64
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: "native.sleep", Source: "native", Name: "sleep", Description: "Sleep.", InputSchema: schema},
		Validator:  validator,
		Execute: func(ctx context.Context, args map[string]any, execution tools.ExecutionContext) (string, error) {
			now := active.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			milliseconds, _ := args["ms"].(json.Number).Int64()
			defer active.Add(-1)
			select {
			case <-time.After(time.Duration(milliseconds) * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "done " + execution.CallID, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog, &peak
}

func parallelCallsStreamer(t *testing.T, sleeps []int) *multiRoundStreamer {
	t.Helper()
	first := make([]responses.Event, 0, len(sleeps)+1)
	for index, ms := range sleeps {
		item := fmt.Sprintf(`{"type":"function_call","call_id":"call-%d","name":"sleep","arguments":"{\"ms\":%d}"}`, index, ms)
		first = append(first, responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item))})
	}
	first = append(first, responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-calls","status":"completed","output":[]}}`)})
	return &multiRoundStreamer{rounds: [][]responses.Event{
		first,
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"all finished"}]}}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[]}}`)},
		},
	}}
}

func TestRunnerPublishesParallelToolResultsInCompletionOrderAndPersistsInCallOrder(t *testing.T) {
	catalog, peak := newSleepCatalog(t)
	// Call 0 is the slowest: published results follow completion order.
	streamer := parallelCallsStreamer(t, []int{300, 20, 150, 80})
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var published []string
	_, err = runner.Run(context.Background(), json.RawMessage(`"run four"`), RunOptions{
		ToolCatalog:          catalog,
		MaxToolRounds:        2,
		MaxParallelToolCalls: 8,
		OnToolResult: func(item json.RawMessage) {
			mu.Lock()
			published = append(published, functionCallID(item))
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if peak.Load() != 4 {
		t.Fatalf("peak concurrency = %d, want all 4 calls in flight together", peak.Load())
	}
	if want := []string{"call-1", "call-3", "call-2", "call-0"}; !reflect.DeepEqual(published, want) {
		t.Fatalf("published results = %v, want completion order %v", published, want)
	}
	// The transcript keeps call order and holds each result exactly once.
	var persisted []string
	if err := journal.ReadLines(func(line []byte) error {
		if strings.Contains(string(line), `"type":"function_call_output"`) {
			persisted = append(persisted, functionCallID(line))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"call-0", "call-1", "call-2", "call-3"}; !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted results = %v, want call order %v", persisted, want)
	}
}

func TestRunnerLimitOneRunsParallelToolCallsOneAfterAnother(t *testing.T) {
	catalog, peak := newSleepCatalog(t)
	streamer := parallelCallsStreamer(t, []int{20, 20, 20})
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var published []string
	if _, err := runner.Run(context.Background(), json.RawMessage(`"run three"`), RunOptions{
		ToolCatalog:          catalog,
		MaxToolRounds:        2,
		MaxParallelToolCalls: 1,
		OnToolResult:         func(item json.RawMessage) { published = append(published, functionCallID(item)) },
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency = %d, want 1", peak.Load())
	}
	if want := []string{"call-0", "call-1", "call-2"}; !reflect.DeepEqual(published, want) {
		t.Fatalf("published = %v, want %v", published, want)
	}
}

func TestRunnerCancellationPersistsEveryParallelCallAndStopsPromptly(t *testing.T) {
	catalog, _ := newSleepCatalog(t)
	// Sleeps far longer than the test; the cancelled context must not wait for them.
	streamer := parallelCallsStreamer(t, []int{20, 5_000, 5_000})
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	began := time.Now()
	_, err = runner.Run(ctx, json.RawMessage(`"run slow"`), RunOptions{
		ToolCatalog:          catalog,
		MaxToolRounds:        2,
		MaxParallelToolCalls: 8,
		// Cancel as soon as the fast call has published its result.
		OnToolResult: func(json.RawMessage) { once.Do(cancel) },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("Run() took %v after cancellation, want it to stop without waiting for the 5s calls", elapsed)
	}
	// Every call of the round has a persisted result: the fast one real, the others
	// recorded as cancelled, so the next run does not see a dangling call.
	var persisted []string
	if err := journal.ReadLines(func(line []byte) error {
		if strings.Contains(string(line), `"type":"function_call_output"`) {
			persisted = append(persisted, functionCallID(line))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 3 {
		t.Fatalf("persisted tool results = %v, want one per call", persisted)
	}
}

// TestRunWithPersistedInputSendsTheSameRequestsAsRunPersistingItsInput checks
// that Run reusing the history handle PersistInput read with changes no
// provider request: the session holds a finished turn and a tool call that
// never got a result, and the run repairs it, appends the new input and
// continues through a tool round.
func TestRunWithPersistedInputSendsTheSameRequestsAsRunPersistingItsInput(t *testing.T) {
	toolCall := json.RawMessage(`{"type":"function_call","call_id":"call-search","name":"lookup","arguments":"{\"query\":\"hello\"}"}`)
	newStreamer := func() *multiRoundStreamer {
		return &multiRoundStreamer{rounds: [][]responses.Event{
			{
				{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + string(toolCall) + `}`)},
				{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)},
			},
			{
				{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)},
				{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[]}}`)},
			},
		}}
	}
	seed := func(store *transcript.TurnStore, journal *transcript.Journal) {
		t.Helper()
		if err := store.AppendInputItems([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"earlier"}]}`)); err != nil {
			t.Fatal(err)
		}
		if err := journal.Append([]byte(`{"type":"response.completed","response":{"id":"resp-0","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"earlier answer"}]}]}}`)); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendInputItems([]byte(`{"type":"function_call","call_id":"call-interrupted","name":"lookup","arguments":"{}"}`)); err != nil {
			t.Fatal(err)
		}
	}
	input := json.RawMessage(`"find hello"`)

	directStore, directJournal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	seed(directStore, directJournal)
	directStreamer := newStreamerRunner(t, directStore, newStreamer())
	if _, err := directStreamer.runner.Run(context.Background(), input, RunOptions{ToolCatalog: newLookupCatalog(t), MaxToolRounds: 4}); err != nil {
		t.Fatalf("direct Run() error = %v", err)
	}

	splitStore, splitJournal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	seed(splitStore, splitJournal)
	persisted, err := PersistInput(splitStore, input, nil)
	if err != nil {
		t.Fatalf("PersistInput() error = %v", err)
	}
	splitStreamer := newStreamerRunner(t, splitStore, newStreamer())
	if _, err := splitStreamer.runner.Run(context.Background(), nil, RunOptions{ToolCatalog: newLookupCatalog(t), MaxToolRounds: 4, PersistedInput: persisted}); err != nil {
		t.Fatalf("split Run() error = %v", err)
	}

	if len(directStreamer.streamer.requests) != 2 || len(splitStreamer.streamer.requests) != 2 {
		t.Fatalf("provider requests = %d direct, %d split, want 2 each", len(directStreamer.streamer.requests), len(splitStreamer.streamer.requests))
	}
	for round := range directStreamer.streamer.requests {
		if string(directStreamer.streamer.requests[round]) != string(splitStreamer.streamer.requests[round]) {
			t.Fatalf("round %d request differs:\ndirect: %s\nsplit:  %s", round, directStreamer.streamer.requests[round], splitStreamer.streamer.requests[round])
		}
	}
	var firstRequest struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(splitStreamer.streamer.requests[0], &firstRequest); err != nil {
		t.Fatal(err)
	}
	if len(firstRequest.Input) != 5 {
		t.Fatalf("first request input items = %d, want earlier turn (2), interrupted call, repaired output and new input", len(firstRequest.Input))
	}
}

type streamerRunner struct {
	streamer *multiRoundStreamer
	runner   *Runner
}

func newStreamerRunner(t *testing.T, store *transcript.TurnStore, streamer *multiRoundStreamer) streamerRunner {
	t.Helper()
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	return streamerRunner{streamer: streamer, runner: runner}
}
