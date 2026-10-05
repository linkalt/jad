package webui

import (
	"encoding/json"
	"testing"
)

func TestStreamingRunNormalizesReasoningToolsAndMultipleResponses(t *testing.T) {
	run := NewStreamingRun("turn-1", "10:10 AM")
	if _, changed := run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-1"}}`)); changed {
		t.Fatal("response.created unexpectedly changed visible message state")
	}

	message := updateStreamingRun(t, run, `{"type":"response.reasoning_summary_text.delta","delta":"Planning."}`)
	if len(message.Parts) != 1 || message.Parts[0].Type != "reasoning" || message.ReasoningContent != "Planning." {
		t.Fatalf("reasoning update = %+v", message)
	}

	message = updateStreamingRun(t, run, `{"type":"response.output_item.added","output_index":0,"item":{"id":"item-1","type":"function_call","call_id":"call-1","name":"write","arguments":""}}`)
	if len(message.Parts) != 2 || message.Parts[1].Type != "tool_call" || !message.Parts[1].Partial {
		t.Fatalf("tool announcement = %+v", message.Parts)
	}

	message = updateStreamingRun(t, run, `{"type":"response.function_call_arguments.delta","item_id":"item-1","output_index":0,"delta":"{\"path\":"}`)
	if message.Parts[1].Arguments == nil || *message.Parts[1].Arguments != `{"path":` || !message.Parts[1].Partial {
		t.Fatalf("tool argument delta = %+v", message.Parts[1])
	}
	message = updateStreamingRun(t, run, `{"type":"response.function_call_arguments.done","item_id":"item-1","output_index":0,"arguments":"{\"path\":\"notes.txt\"}"}`)
	if message.Parts[1].Arguments == nil || *message.Parts[1].Arguments != `{"path":"notes.txt"}` || message.Parts[1].Partial {
		t.Fatalf("tool arguments done = %+v", message.Parts[1])
	}

	message = updateStreamingRun(t, run, `{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","call_id":"call-1","name":"write","arguments":"{\"path\":\"notes.txt\"}"}]}}`)
	if len(message.Parts) != 2 || message.Parts[1].Partial {
		t.Fatalf("completed first response parts = %+v", message.Parts)
	}

	message, changed := run.AddToolResult(json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"{\"status\":\"ok\"}\nwritten"}`))
	if !changed || len(message.Parts) != 3 || message.Parts[2].Type != "tool_result" || message.Parts[2].Status != "success" {
		t.Fatalf("tool result update = changed %v, parts %+v", changed, message.Parts)
	}

	if _, changed := run.Update(json.RawMessage(`{"type":"response.created","response":{"id":"resp-2"}}`)); changed {
		t.Fatal("second response.created unexpectedly changed visible message state")
	}
	updateStreamingRun(t, run, `{"type":"response.output_text.delta","delta":"Done."}`)
	final := run.Complete(json.RawMessage(`{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}]}}`))
	if final.Status != "complete" || final.Text != "Done." || final.ReasoningContent != "Planning." {
		t.Fatalf("final message summaries = %+v", final)
	}
	if len(final.Parts) != 4 || final.Parts[0].Type != "reasoning" || final.Parts[1].Type != "tool_call" || final.Parts[2].Type != "tool_result" || final.Parts[3].Type != "text" {
		t.Fatalf("final parts = %+v", final.Parts)
	}
	if len(final.Steps) != 2 || final.Steps[0].ID != "resp-1" || final.Steps[1].ID != "resp-2" || !final.Steps[0].HasTools {
		t.Fatalf("final response steps = %+v", final.Steps)
	}
}

func TestToolResultPartMarksMCPErrorAndMultimodalContent(t *testing.T) {
	part, ok := ToolResultPart(json.RawMessage(`{"type":"function_call_output","call_id":"call-mcp","output":[{"type":"input_text","text":"MCP server reported a tool error."},{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"input_file","filename":"report.pdf"}]}`), 0)
	if !ok || part.Type != "tool_result" || part.Status != "error" {
		t.Fatalf("MCP error result = %+v, ok=%v", part, ok)
	}
	if want := "MCP server reported a tool error.\n[MCP image content]\n[MCP file: report.pdf]"; part.Output != want {
		t.Fatalf("MCP result output = %q, want %q", part.Output, want)
	}
}

func updateStreamingRun(t *testing.T, run *StreamingRun, event string) Message {
	t.Helper()
	message, changed := run.Update(json.RawMessage(event))
	if !changed {
		t.Fatalf("event did not change visible message: %s", event)
	}
	return message
}
