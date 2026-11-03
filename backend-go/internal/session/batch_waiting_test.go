package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"jad/backend-go/internal/tools"
)

// TestToolRoundTellsACallWhenLaterCallsWaitBehindIt: in a batch that runs fewer calls at once
// than it has, every call but the last is told that later calls wait for it (BatchWaiting), so
// subagent and profile_message do not hold them back for their long default block. A batch that
// runs all its calls together tells none.
func TestToolRoundTellsACallWhenLaterCallsWaitBehindIt(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	tool := func(name string, writes bool) tools.Tool {
		tool := tools.Tool{
			Definition: tools.Definition{ID: "native:" + name, Name: name, Source: "native", Description: name, InputSchema: schema},
			ReadOnly:   true, Validator: validator,
			Execute: func(_ context.Context, _ map[string]any, execution tools.ExecutionContext) (string, error) {
				mu.Lock()
				seen[execution.CallID] = execution.BatchWaiting
				mu.Unlock()
				return "ok", nil
			},
		}
		if writes {
			tool.MayChangeFiles = func(map[string]any) bool { return true }
		}
		return tool
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool("message", true), tool("look", false)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, name string) json.RawMessage {
		return json.RawMessage(`{"type":"function_call","call_id":"` + id + `","name":"` + name + `","arguments":"{}"}`)
	}
	for _, tc := range []struct {
		name       string
		calls      []json.RawMessage
		limit      int
		scheduling string
		want       map[string]bool
	}{
		{"ordered with a writer runs one at a time", []json.RawMessage{call("a", "message"), call("b", "look"), call("c", "message")}, 8, "ordered",
			map[string]bool{"a": true, "b": true, "c": false}},
		{"read-only calls run together", []json.RawMessage{call("a", "look"), call("b", "look")}, 8, "ordered",
			map[string]bool{"a": false, "b": false}},
		{"concurrent scheduling runs writers together", []json.RawMessage{call("a", "message"), call("b", "message")}, 8, "concurrent",
			map[string]bool{"a": false, "b": false}},
		{"fewer slots than calls", []json.RawMessage{call("a", "look"), call("b", "look"), call("c", "look")}, 2, "concurrent",
			map[string]bool{"a": true, "b": true, "c": false}},
		{"a single call", []json.RawMessage{call("a", "message")}, 8, "ordered",
			map[string]bool{"a": false}},
	} {
		mu.Lock()
		clear(seen)
		mu.Unlock()
		if _, err := executeToolRound(context.Background(), catalog, tc.calls, tools.ExecutionContext{}, tc.limit, tc.scheduling, nil, nil); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		mu.Lock()
		for id, want := range tc.want {
			if got, ran := seen[id]; !ran || got != want {
				t.Errorf("%s: call %s BatchWaiting = %v (ran %v), want %v", tc.name, id, got, ran, want)
			}
		}
		mu.Unlock()
	}
}
