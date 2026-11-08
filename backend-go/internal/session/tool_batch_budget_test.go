package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"jad/backend-go/internal/tools"
)

// leafBudgetTimeout bounds every budget test: a leaf that never gets a token would
// otherwise hang the test instead of failing it.
const leafBudgetTimeout = 10 * time.Second

// leafTool is a tool whose calls block until release is closed, and whose executed
// calls are counted. It opts into the leaf budget.
func leafTool(t *testing.T, release <-chan struct{}, executed chan<- string) tools.Tool {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	return tools.Tool{
		Definition: tools.Definition{ID: "native.leaf", Source: "native", Name: "leaf", Description: "Leaf.", InputSchema: schema},
		Validator:  validator,
		LeafBudget: true,
		Execute: func(ctx context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			executed <- execution.CallID
			select {
			case <-release:
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
}

func leafCall(id string) json.RawMessage {
	return json.RawMessage(`{"type":"function_call","call_id":"` + id + `","name":"leaf","arguments":"{}"}`)
}

// runWithin runs fn and fails the test if it does not return within leafBudgetTimeout.
func runWithin(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(leafBudgetTimeout):
		t.Fatalf("%s did not finish within %s: a leaf waited on a token that was never released", name, leafBudgetTimeout)
	}
}

// TestLeafHoldsATokenOnlyWhileItRuns: with budget 1 the second leaf starts only after
// the first settles, so the token is given back when the call finishes.
func TestLeafHoldsATokenOnlyWhileItRuns(t *testing.T) {
	budget := tools.NewLeafBudget(1)
	release := make(chan struct{})
	executed := make(chan string, 4)
	catalog, err := tools.NewCatalog([]tools.Tool{leafTool(t, release, executed)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = executeToolRound(context.Background(), catalog, []json.RawMessage{leafCall("a"), leafCall("b")}, tools.ExecutionContext{}, 8, "", budget, nil)
	}()
	first := <-executed
	select {
	case second := <-executed:
		t.Fatalf("%s and %s ran together with budget 1", first, second)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-executed:
	case <-time.After(leafBudgetTimeout):
		t.Fatal("the second leaf never started after the first settled")
	}
	<-done
}

// TestLeafTokenIsReleasedOnError: a leaf that fails still gives its token back, so the
// next leaf can run.
func TestLeafTokenIsReleasedOnError(t *testing.T) {
	budget := tools.NewLeafBudget(1)
	calls := make(chan string, 4)
	failing := tools.Tool{
		Definition: tools.Definition{ID: "native.leaf", Source: "native", Name: "leaf", Description: "Leaf.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		LeafBudget: true,
		Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
			calls <- execution.CallID
			return "", errors.New("boom")
		},
	}
	validator, err := tools.CompileSchema(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	failing.Validator = validator
	catalog, err := tools.NewCatalog([]tools.Tool{failing})
	if err != nil {
		t.Fatal(err)
	}
	runWithin(t, "two failing leaves", func() {
		_, _ = executeToolRound(context.Background(), catalog, []json.RawMessage{leafCall("a"), leafCall("b")}, tools.ExecutionContext{}, 8, "", budget, nil)
	})
	if len(calls) != 2 {
		t.Fatalf("%d leaves ran after an error, want 2", len(calls))
	}
}

// TestLeafTokenIsReleasedOnContextCancel: a leaf cancelled by its context gives its
// token back, so a leaf started after the cancel can run.
func TestLeafTokenIsReleasedOnContextCancel(t *testing.T) {
	budget := tools.NewLeafBudget(1)
	release := make(chan struct{})
	executed := make(chan string, 4)
	catalog, err := tools.NewCatalog([]tools.Tool{leafTool(t, release, executed)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = executeToolRound(ctx, catalog, []json.RawMessage{leafCall("a")}, tools.ExecutionContext{}, 8, "", budget, nil)
	}()
	<-executed
	cancel()
	runWithin(t, "cancelled leaf", func() { <-done })
	// The token is free again: a fresh leaf takes it and finishes.
	close(release)
	runWithin(t, "leaf after cancel", func() {
		_, _ = executeToolRound(context.Background(), catalog, []json.RawMessage{leafCall("b")}, tools.ExecutionContext{}, 8, "", budget, nil)
	})
}
