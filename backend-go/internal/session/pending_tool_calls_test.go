package session

import (
	"encoding/json"
	"reflect"
	"testing"
)

func legacyPendingToolCalls(history [][]byte) []json.RawMessage {
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
			if callID := functionCallID(item); callID != "" {
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
	return calls
}

func TestPendingToolCallsMatchesLegacyScan(t *testing.T) {
	call := func(id string) string {
		return `{"type":"function_call","call_id":"` + id + `","name":"x","arguments":"{}"}`
	}
	output := func(id string) string { return `{"type":"function_call_output","call_id":"` + id + `","output":"ok"}` }
	cases := map[string][]string{
		"empty":               {},
		"none pending":        {call("a"), output("a")},
		"one pending":         {`{"type":"message"}`, call("a")},
		"multiple pending":    {call("b"), call("a"), output("c"), call("c"), call("d")},
		"output before call":  {output("a"), call("a")},
		"duplicate call":      {call("a"), `{"type":"function_call","call_id":"a","name":"second"}`, call("b")},
		"duplicate then done": {call("a"), call("a"), output("a")},
		"malformed items":     {`not json`, `[1]`, `null`, `{"type":5,"call_id":"a"}`, `{"type":"function_call","call_id":7}`, `{"type":"function_call"}`, `{"type":"function_call","call_id":""}`, `{"call_id":"a"}`, call("ok")},
		"nested call ids":     {`{"type":"message","content":[{"type":"function_call","call_id":"n"}]}`, call("a")},
		"unrelated output":    {call("a"), `{"type":"function_call_output","call_id":"zzz"}`, `{"type":"function_call_output"}`},
		"duplicate keys":      {`{"type":"function_call","call_id":"a","call_id":"b"}`, `{"type":"message","type":"function_call","call_id":"c"}`},
	}
	for name, raws := range cases {
		t.Run(name, func(t *testing.T) {
			history := make([][]byte, len(raws))
			for index, raw := range raws {
				history[index] = []byte(raw)
			}
			got, want := pendingToolCalls(history), legacyPendingToolCalls(history)
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pendingToolCalls() = %s, want %s", got, want)
			}
		})
	}
}
