package webui

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestStreamingRunSummariesMatchTheParts: the run keeps the message text and reasoning
// up to date as deltas arrive instead of joining every part per snapshot. After every
// event of a mixed run (several steps, tool calls, tool results, hosted tools, completed
// responses that rewrite text, deltas after completion) they must equal a full join.
func TestStreamingRunSummariesMatchTheParts(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.reasoning_text.delta","delta":"  Plan"}`,
		`{"type":"response.reasoning_text.delta","delta":" it "}`,
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.output_text.delta","delta":" world"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"read","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{\"a\":"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc1","arguments":"{\"a\":1}"}`,
		`{"type":"response.completed","response":{"id":"r1","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Plan it fully"}]},{"type":"message","content":[{"type":"output_text","text":"Hello world!"}]},{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"a\":1}"}]}}`,
		`tool:{"type":"function_call_output","call_id":"c1","output":"{\"status\":\"ok\"}"}`,
		`{"type":"response.created","response":{"id":"r2"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Second"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":" thought\n"}`,
		`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws1","status":"in_progress","action":{"query":"q"}}}`,
		`{"type":"response.output_text.delta","delta":"Found"}`,
		`{"type":"response.output_text.delta","delta":" it"}`,
		`{"type":"response.completed","response":{"id":"r2","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Second thought"}]},{"type":"web_search_call","id":"ws1","status":"completed","action":{"query":"q"}},{"type":"message","content":[{"type":"output_text","text":"Found"},{"type":"output_text","text":" it, twice"}]}]}}`,
		`{"type":"response.output_text.delta","delta":" late"}`,
		`{"type":"response.reasoning_text.delta","delta":"late thinking"}`,
		`{"type":"response.output_text.delta","delta":" text"}`,
	}
	run := NewStreamingRun("turn", "now")
	for index, event := range events {
		var message Message
		var changed bool
		if raw, ok := strings.CutPrefix(event, "tool:"); ok {
			message, changed = run.AddToolResult(json.RawMessage(raw))
		} else {
			message, changed = run.Update(json.RawMessage(event))
		}
		if !changed {
			continue
		}
		checkSummaries(t, fmt.Sprintf("event %d", index), message)
	}
	checkSummaries(t, "complete", run.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"r2","output":[{"type":"message","content":[{"type":"output_text","text":"Found it, twice late text"}]}]}}`)))
}

// TestStreamingRunSummariesMatchThePartsInRandomRuns drives the same check with random
// sequences of the events that touch text, reasoning and the parts between them.
func TestStreamingRunSummariesMatchThePartsInRandomRuns(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	kinds := []func(int) string{
		func(int) string { return `{"type":"response.output_text.delta","delta":"t "}` },
		func(int) string { return `{"type":"response.reasoning_text.delta","delta":" r\n"}` },
		func(int) string { return `{"type":"response.reasoning_summary_text.delta","delta":"s"}` },
		func(step int) string {
			return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":"fc%d","call_id":"c%d","name":"n","arguments":""}}`, step, step, step)
		},
		func(step int) string {
			return fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,"delta":"x"}`, step-1)
		},
		func(step int) string {
			return fmt.Sprintf(`tool:{"type":"function_call_output","call_id":"c%d","output":"out"}`, step-1)
		},
		func(step int) string { return fmt.Sprintf(`{"type":"response.created","response":{"id":"r%d"}}`, step) },
		func(step int) string {
			return fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"done %d"}]},{"type":"message","content":[{"type":"output_text","text":"final %d"}]}]}}`, step, step, step)
		},
	}
	for sequence := 0; sequence < 200; sequence++ {
		run := NewStreamingRun("turn", "now")
		for step := 0; step < 60; step++ {
			event := kinds[random.Intn(len(kinds))](step)
			var message Message
			var changed bool
			if raw, ok := strings.CutPrefix(event, "tool:"); ok {
				message, changed = run.AddToolResult(json.RawMessage(raw))
			} else {
				message, changed = run.Update(json.RawMessage(event))
			}
			if changed {
				checkSummaries(t, fmt.Sprintf("sequence %d step %d", sequence, step), message)
			}
			if t.Failed() {
				return
			}
		}
	}
}

func checkSummaries(t *testing.T, label string, message Message) {
	t.Helper()
	text, reasoning := FieldsFromParts(message.Parts)
	if message.Text != text || message.ReasoningContent != reasoning {
		t.Errorf("%s: summaries = %q / %q, want %q / %q", label, message.Text, message.ReasoningContent, text, reasoning)
	}
}
