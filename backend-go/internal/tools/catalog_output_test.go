package tools

import (
	"context"
	"encoding/json"
	"strings"
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

// The envelope has to keep the characters the value was encoded with. json.Marshal of a
// struct holding a json.RawMessage compacts it with HTML escaping on, which used to turn a
// raw <, >, & and the U+2028/U+2029 separators back into \u003c, \u0026, \u2028 and \u2029.
func TestFunctionCallOutputEnvelopeKeepsTheEncodedCharacters(t *testing.T) {
	value, err := MarshalJSON(map[string]string{"x": "a<b&c>d\u2028e\u2029f"})
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	envelope, err := encodeFunctionCallOutputValue("call-1", value)
	if err != nil {
		t.Fatalf("encodeFunctionCallOutputValue() error = %v", err)
	}
	want := `{"type":"function_call_output","call_id":"call-1","output":{"x":"a<b&c>d` + "\u2028" + `e` + "\u2029" + `f"}}`
	if string(envelope) != want {
		t.Fatalf("envelope = %s, want %s", envelope, want)
	}
	if !json.Valid(envelope) {
		t.Fatalf("envelope is not valid JSON: %s", envelope)
	}
	text, err := encodeFunctionCallOutput("call-1", "a<b&c")
	if err != nil {
		t.Fatalf("encodeFunctionCallOutput() error = %v", err)
	}
	if !strings.Contains(string(text), `"a<b&c"`) {
		t.Fatalf("string output = %s, want the characters unescaped", text)
	}
}

// MarshalJSON is the only encoder for bytes that reach a json.RawMessage, so its form is
// what the model and messages.jsonl see.
func TestMarshalJSONKeepsAnglesAndSeparators(t *testing.T) {
	encoded, err := MarshalJSON([]string{"<", ">", "&", "\u2028", "\u2029", `\u2028`})
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	want := `["<",">","&","` + "\u2028" + `","` + "\u2029" + `","\\u2028"]`
	if string(encoded) != want {
		t.Fatalf("MarshalJSON() = %s, want %s", encoded, want)
	}
}
