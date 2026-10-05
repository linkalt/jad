package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCatalogPreservesJSONValueOutput(t *testing.T) {
	tool := testTool(t)
	tool.Definition.ID = "mcp:fixture:echo"
	tool.Definition.Source = "mcp"
	tool.Definition.Name = "mcp_fixture_echo"
	tool.Execute = nil
	tool.ExecuteOutput = func(context.Context, map[string]any, ExecutionContext) (json.RawMessage, error) {
		return json.RawMessage(`[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"auto"}]`), nil
	}
	catalog := newTestCatalog(t, tool)
	output, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "call-mcp", tool.Definition.Name, `{"command":"hello"}`), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	var decoded struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("Unmarshal(function output) error = %v", err)
	}
	if decoded.Type != "function_call_output" || decoded.CallID != "call-mcp" {
		t.Fatalf("function output envelope = %+v", decoded)
	}
	if string(decoded.Output) != string(toolOutputFixture) {
		t.Fatalf("JSON output = %s, want %s", decoded.Output, toolOutputFixture)
	}
}

var toolOutputFixture = json.RawMessage(`[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"auto"}]`)
