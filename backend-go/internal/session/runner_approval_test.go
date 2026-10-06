package session

import (
	"context"
	"encoding/json"
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

// newApprovalCatalog has two tools: "gated" asks for approval, "free" does not.
// ran counts how often gated actually executed.
func newApprovalCatalog(t *testing.T) (*tools.Catalog, *atomic.Int64) {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int64
	build := func(name, approval string, counted bool) tools.Tool {
		return tools.Tool{
			Definition: tools.Definition{ID: "native." + name, Source: "native", Name: name, Description: name, InputSchema: schema},
			Validator:  validator,
			Approval:   approval,
			Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
				if counted {
					ran.Add(1)
				}
				return "ran " + execution.CallID, nil
			},
		}
	}
	catalog, err := tools.NewCatalog([]tools.Tool{build("gated", tools.ApprovalAsk, true), build("free", "", false)})
	if err != nil {
		t.Fatal(err)
	}
	return catalog, &ran
}

func approvalCallsStreamer(calls ...[2]string) *multiRoundStreamer {
	first := make([]responses.Event, 0, len(calls)+1)
	for index, call := range calls {
		item := fmt.Sprintf(`{"type":"function_call","call_id":"%s","name":"%s","arguments":"{\"command\":\"ls\"}"}`, call[0], call[1])
		first = append(first, responses.Event{Type: "response.output_item.done", Raw: json.RawMessage(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item))})
	}
	first = append(first, responses.Event{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-calls","status":"completed","output":[]}}`)})
	return &multiRoundStreamer{rounds: [][]responses.Event{
		first,
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"understood"}]}}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[]}}`)},
		},
	}}
}

func approvalOptions() *tools.ApprovalOptions {
	return &tools.ApprovalOptions{TimeoutMS: 1000, ExtendMS: 1000, MaxTimeoutMS: 5000, PreviewBytes: 4096}
}

// persistedOutputs returns the transcript's function_call_output items by call id.
func persistedOutputs(t *testing.T, journal interface {
	ReadLines(func([]byte) error) error
}) map[string]string {
	t.Helper()
	outputs := map[string]string{}
	if err := journal.ReadLines(func(line []byte) error {
		if !strings.Contains(string(line), `"type":"function_call_output"`) {
			return nil
		}
		var item struct {
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal(line, &item); err != nil {
			return err
		}
		outputs[item.CallID] = item.Output
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return outputs
}

func TestRunnerHoldsAGatedCallForTheUserWhileAFreeCallFinishes(t *testing.T) {
	catalog, ran := newApprovalCatalog(t)
	streamer := approvalCallsStreamer([2]string{"call-gated", "gated"}, [2]string{"call-free", "free"})
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan tools.InteractionQuestion, 1)
	release := make(chan tools.InteractionAnswer)
	var mu sync.Mutex
	var published []string
	var gatedRanWhileWaiting bool
	freePublished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), json.RawMessage(`"list"`), RunOptions{
			ToolCatalog:          catalog,
			MaxToolRounds:        2,
			MaxParallelToolCalls: 8,
			Approval:             approvalOptions(),
			ToolExecution: tools.ExecutionContext{RequestInteraction: func(_ context.Context, identity tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
				if identity.Tool != "gated" || identity.ToolCallID != "call-gated" {
					return tools.InteractionAnswer{}, fmt.Errorf("unexpected identity %+v", identity)
				}
				asked <- question
				return <-release, nil
			}},
			OnToolResult: func(item json.RawMessage) {
				mu.Lock()
				published = append(published, functionCallID(item))
				if functionCallID(item) == "call-free" {
					close(freePublished)
				}
				mu.Unlock()
			},
		})
		done <- err
	}()
	var question tools.InteractionQuestion
	select {
	case question = <-asked:
	case <-time.After(3 * time.Second):
		t.Fatal("the gated call never asked")
	}
	if question.Kind != tools.InteractionKindApproval || question.Title != "Run gated" || !strings.Contains(string(question.Draft), `"arguments":"{\"command\":\"ls\"}"`) {
		t.Fatalf("question = %+v draft %s", question, question.Draft)
	}
	select {
	case <-freePublished:
	case <-time.After(3 * time.Second):
		t.Fatal("the free call was held up by the question waiting on the gated one")
	}
	if ran.Load() != 0 {
		gatedRanWhileWaiting = true
	}
	release <- tools.InteractionAnswer{Decision: tools.InteractionAccept, Outcome: tools.InteractionAnswered, Source: tools.InteractionSourceUser}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not finish after the answer")
	}
	if gatedRanWhileWaiting || ran.Load() != 1 {
		t.Fatalf("gated ran while waiting=%v, total runs=%d; want it to run exactly once, after the answer", gatedRanWhileWaiting, ran.Load())
	}
	if !reflect.DeepEqual(published, []string{"call-free", "call-gated"}) {
		t.Fatalf("published = %v, want the free call first", published)
	}
	// The transcript keeps call order and one result per call.
	outputs := persistedOutputs(t, journal)
	if len(outputs) != 2 || !strings.Contains(outputs["call-gated"], "ran call-gated") || !strings.Contains(outputs["call-free"], "ran call-free") {
		t.Fatalf("persisted outputs = %v", outputs)
	}
}

func TestRunnerTurnsADeclineIntoABlockedResultAndGoesOn(t *testing.T) {
	catalog, ran := newApprovalCatalog(t)
	streamer := approvalCallsStreamer([2]string{"call-gated", "gated"})
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	reason := "use a copy instead"
	if _, err := runner.Run(context.Background(), json.RawMessage(`"list"`), RunOptions{
		ToolCatalog:   catalog,
		MaxToolRounds: 2,
		Approval:      approvalOptions(),
		ToolExecution: tools.ExecutionContext{RequestInteraction: func(context.Context, tools.InteractionIdentity, tools.InteractionQuestion) (tools.InteractionAnswer, error) {
			return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionAnswered, Reason: &reason, Source: tools.InteractionSourceUser}, nil
		}},
	}); err != nil {
		t.Fatalf("a declined call must not fail the run: %v", err)
	}
	if ran.Load() != 0 {
		t.Fatal("a declined call ran")
	}
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want the model to get a second round to re-plan", len(streamer.requests))
	}
	output := persistedOutputs(t, journal)["call-gated"]
	for _, want := range []string{`"status":"blocked"`, "approval_denied", "Not run: " + reason} {
		if !strings.Contains(output, want) {
			t.Fatalf("persisted output lacks %q:\n%s", want, output)
		}
	}
}

func TestRunnerHeadlessRunBlocksOrAllowsAGatedCall(t *testing.T) {
	cases := []struct {
		name      string
		approval  *tools.ApprovalOptions
		wantRan   int64
		wantInOut string
	}{
		{"deny", approvalOptions(), 0, "approval_unavailable"},
		{"allow", func() *tools.ApprovalOptions { o := approvalOptions(); o.HeadlessAllow = true; return o }(), 1, "ran call-gated"},
		{"the host supplied no settings", nil, 0, "approval_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog, ran := newApprovalCatalog(t)
			streamer := approvalCallsStreamer([2]string{"call-gated", "gated"})
			store, journal, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
			runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
			if err != nil {
				t.Fatal(err)
			}
			// No RequestInteraction: nobody can answer.
			if _, err := runner.Run(context.Background(), json.RawMessage(`"list"`), RunOptions{ToolCatalog: catalog, MaxToolRounds: 2, Approval: tc.approval}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ran.Load() != tc.wantRan || !strings.Contains(persistedOutputs(t, journal)["call-gated"], tc.wantInOut) {
				t.Fatalf("ran=%d output=%q; want ran=%d containing %q", ran.Load(), persistedOutputs(t, journal)["call-gated"], tc.wantRan, tc.wantInOut)
			}
		})
	}
}

func TestRunnerStoppedRunDoesNotRunAGatedCall(t *testing.T) {
	catalog, ran := newApprovalCatalog(t)
	streamer := approvalCallsStreamer([2]string{"call-gated", "gated"})
	store, _, _ := newTestStore(t, transcript.ResponseMetadataMinimal)
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	asked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, json.RawMessage(`"list"`), RunOptions{
			ToolCatalog: catalog, MaxToolRounds: 2, Approval: approvalOptions(),
			ToolExecution: tools.ExecutionContext{RequestInteraction: func(ctx context.Context, _ tools.InteractionIdentity, _ tools.InteractionQuestion) (tools.InteractionAnswer, error) {
				close(asked)
				<-ctx.Done()
				reason := "The run was stopped before the draft was answered."
				return tools.InteractionAnswer{Decision: tools.InteractionDecline, Outcome: tools.InteractionCancelled, Reason: &reason}, nil
			}},
		})
		done <- err
	}()
	select {
	case <-asked:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("the gated call never asked")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stopped run did not return")
	}
	if ran.Load() != 0 {
		t.Fatal("the gated call ran although the run was stopped before it was approved")
	}
}
