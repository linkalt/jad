package chatadapter

import (
	"fmt"
	"strings"
	"testing"
)

// outputOrder returns the completed response's output as "type@index" in output order,
// where the index comes from the id the translator derived from the output_index, and
// the output_index of each output_item.done event in the order the stream sent them.
func outputOrder(t *testing.T, events []event) (completed []string, doneOrder []string) {
	t.Helper()
	for _, one := range events {
		if one.Type == "response.output_item.done" {
			item := one.Data["item"].(map[string]any)
			doneOrder = append(doneOrder, fmt.Sprintf("%v@%v", item["type"], one.Data["output_index"]))
		}
	}
	response := events[len(events)-1].Data["response"].(map[string]any)
	for _, raw := range response["output"].([]any) {
		item := raw.(map[string]any)
		id := item["id"].(string)
		completed = append(completed, fmt.Sprintf("%v@%s", item["type"], id[strings.LastIndex(id, "_")+1:]))
	}
	return completed, doneOrder
}

// Text that follows a tool call opens a new item while the call is still open. The
// call keeps the lower output_index and is closed last, so the completed output must
// not follow the close order.
func TestStreamCompletedOutputFollowsOutputIndexWhenTextFollowsAToolCall(t *testing.T) {
	cases := []struct {
		name          string
		deltas        []string
		wantCompleted string
		wantDone      string
	}{
		{
			name: "call then text",
			deltas: []string{
				`{"id":"chatcmpl-o","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"content":"after"}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			wantCompleted: "function_call@0,message@1",
			wantDone:      "message@1,function_call@0",
		},
		{
			name: "text, call, text, second call",
			deltas: []string{
				`{"id":"chatcmpl-o","model":"m","choices":[{"index":0,"delta":{"content":"first"}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"content":"middle"}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"list","arguments":"{}"}}]}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			wantCompleted: "message@0,function_call@1,message@2,function_call@3",
			wantDone:      "message@0,message@2,function_call@1,function_call@3",
		},
		{
			name: "reasoning, call, text",
			deltas: []string{
				`{"id":"chatcmpl-o","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{"content":"tail"}}]}`,
				`{"id":"chatcmpl-o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			wantCompleted: "reasoning@0,function_call@1,message@2",
			wantDone:      "reasoning@0,message@2,function_call@1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &fakeUpstream{events: append(append([]string{}, tc.deltas...), `[DONE]`)}
			status, _, events, _ := serve(t, upstream, "", `{"model":"qwen","stream":true,"input":"go"}`)
			if status != 200 {
				t.Fatalf("status %d", status)
			}
			completed, done := outputOrder(t, events)
			if got := strings.Join(completed, ","); got != tc.wantCompleted {
				t.Fatalf("completed output = %s, want %s", got, tc.wantCompleted)
			}
			// The done events keep the order the stream closed the items in; the
			// runner pairs them with the completed output by id, not by arrival.
			if got := strings.Join(done, ","); got != tc.wantDone {
				t.Fatalf("done events = %s, want %s", got, tc.wantDone)
			}
		})
	}
}
