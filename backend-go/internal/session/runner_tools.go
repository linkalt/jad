package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"example.com/agent-harness/backend-go/internal/tools"
	"example.com/agent-harness/backend-go/internal/transcript"
)

// repairPendingToolCalls gives every tool call without a result a failure
// result. It reads the history through the caller's handle, so the run that
// follows expands only what the repair and the new input append.
func repairPendingToolCalls(store *transcript.TurnStore, reader *transcript.InputHistory) error {
	history, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read session history before repairing pending tool calls: %w", err)
	}
	pending := make(map[string]json.RawMessage)
	order := make([]string, 0)
	for _, item := range history {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		var itemType string
		if json.Unmarshal(fields["type"], &itemType) != nil {
			continue
		}
		switch itemType {
		case "function_call":
			callID := functionCallID(item)
			if callID == "" {
				continue
			}
			if _, exists := pending[callID]; !exists {
				order = append(order, callID)
			}
			pending[callID] = cloneRaw(item)
		case "function_call_output":
			callID := functionCallID(item)
			if callID != "" {
				delete(pending, callID)
			}
		}
	}
	if len(pending) == 0 {
		return nil
	}
	calls := make([]json.RawMessage, 0, len(pending))
	for _, callID := range order {
		if call, exists := pending[callID]; exists {
			calls = append(calls, call)
		}
	}
	_, err = appendToolOutputs(store, calls, nil, "tool_result_missing", "A previous tool call has no persisted result and was not rerun automatically.")
	return err
}

func responseFunctionCalls(event json.RawMessage) ([]json.RawMessage, error) {
	var envelope struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(event, &envelope); err != nil {
		return nil, fmt.Errorf("decode committed response for tool calls: %w", err)
	}
	calls := make([]json.RawMessage, 0)
	for _, item := range envelope.Response.Output {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &header); err != nil {
			continue
		}
		if header.Type == "function_call" {
			calls = append(calls, cloneRaw(item))
		}
	}
	return calls, nil
}

func functionCallID(item json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return ""
	}
	var callID string
	if json.Unmarshal(fields["call_id"], &callID) != nil {
		return ""
	}
	return callID
}

func appendToolOutputs(store *transcript.TurnStore, calls, completed []json.RawMessage, errorCode, summary string) ([]json.RawMessage, error) {
	completedByCallID := make(map[string]json.RawMessage, len(completed))
	for _, output := range completed {
		if callID := functionCallID(output); callID != "" {
			completedByCallID[callID] = cloneRaw(output)
		}
	}
	ordered := make([]json.RawMessage, 0, len(calls))
	for _, call := range calls {
		callID := functionCallID(call)
		if callID == "" {
			continue
		}
		if output, ok := completedByCallID[callID]; ok {
			ordered = append(ordered, output)
			continue
		}
		output, err := missingToolOutput(callID, errorCode, summary)
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, output)
	}
	if len(ordered) == 0 {
		return ordered, nil
	}
	items := make([][]byte, len(ordered))
	for index, output := range ordered {
		items[index] = output
	}
	if err := store.AppendInputItems(items...); err != nil {
		return nil, fmt.Errorf("persist native tool outputs: %w", err)
	}
	return ordered, nil
}

func missingToolOutput(callID, errorCode, summary string) (json.RawMessage, error) {
	result := tools.ExecutionError(callID, tools.StatusError, errorCode, summary)
	encoded, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: callID, Output: result})
	if err != nil {
		return nil, fmt.Errorf("encode missing native tool output: %w", err)
	}
	return encoded, nil
}

// toolRound is what one batch of tool calls produced.
type toolRound struct {
	// Completed holds the output of every call that settled, in call order. Calls
	// deferred behind a user-decision tool appear here with their placeholder
	// output; calls that never ran or failed are absent.
	Completed []json.RawMessage
	// Published is the set of call ids whose output was already handed to the
	// publish callback while the batch was running.
	Published map[string]struct{}
}

// executeToolRound runs the calls of one provider response with at most limit
// of them in flight at once. When the response contains a user-decision tool
// only that call runs and every other call gets a placeholder output.
//
// publish, when set, receives each executed call's output the moment the call
// settles, so completion order (not call order) is what a client sees. Calls
// to publish are serialized. Placeholder outputs are not published here: the
// caller publishes them after the round is persisted, together with anything
// else that is still unpublished.
func executeToolRound(ctx context.Context, catalog *tools.Catalog, calls []json.RawMessage, execution tools.ExecutionContext, limit int, publish func(json.RawMessage)) (toolRound, error) {
	round := toolRound{Published: make(map[string]struct{}, len(calls))}
	decisionIndex := -1
	for index, call := range calls {
		if catalog.IsUserDecisionCall(call) {
			decisionIndex = index
			break
		}
	}

	byCallID := make(map[string]json.RawMessage, len(calls))
	toExecute := make([]json.RawMessage, 0, len(calls))
	for index, call := range calls {
		callID := functionCallID(call)
		if callID == "" {
			return round, errors.New("function call is missing call_id")
		}
		if decisionIndex >= 0 && index != decisionIndex {
			output, err := missingToolOutput(callID, "user_decision_required", "This call was not executed because a user-decision tool in the same response must be resolved first; re-plan after its result.")
			if err != nil {
				return round, err
			}
			byCallID[callID] = output
			continue
		}
		toExecute = append(toExecute, call)
	}

	var mu sync.Mutex
	batchErr := runToolBatch(toExecute, limit,
		func(call json.RawMessage) (json.RawMessage, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return catalog.ExecuteFunctionCall(ctx, call, execution)
		},
		func(_ json.RawMessage, output json.RawMessage) error {
			outputID := functionCallID(output)
			if outputID == "" {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			byCallID[outputID] = output
			round.Published[outputID] = struct{}{}
			if publish != nil {
				publish(cloneRaw(output))
			}
			return nil
		},
		nil,
	)

	round.Completed = make([]json.RawMessage, 0, len(byCallID))
	for _, call := range calls {
		if output, ok := byCallID[functionCallID(call)]; ok {
			round.Completed = append(round.Completed, output)
		}
	}
	return round, batchErr
}
