package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testTool(t *testing.T) Tool {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1}},"required":["command"],"additionalProperties":false}`)
	validator, err := CompileSchema(schema)
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	return Tool{
		Definition: Definition{
			ID:          "native.echo",
			Source:      "native",
			Name:        "echo",
			Description: "Echo one string.",
			InputSchema: schema,
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"command":"hello"}`),
		NormalizeArguments: func(args map[string]any) map[string]any {
			if command, ok := args["command"].(string); ok {
				args["command"] = strings.TrimSpace(command)
			}
			return args
		},
		Execute: func(_ context.Context, args map[string]any, execution ExecutionContext) (string, error) {
			if execution.CallID == "execution-error" {
				return "", errors.New("private error details")
			}
			return "echo:" + args["command"].(string), nil
		},
	}
}

func newTestCatalog(t *testing.T, tools ...Tool) *Catalog {
	t.Helper()
	catalog, err := NewCatalog(tools)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func callJSON(t *testing.T, callID, name string, arguments any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	}{Type: "function_call", CallID: callID, Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("Marshal(function call) error = %v", err)
	}
	return encoded
}

func decodeOutput(t *testing.T, raw json.RawMessage) (string, string) {
	t.Helper()
	var output struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("Unmarshal(function call output) error = %v", err)
	}
	if output.Type != "function_call_output" {
		t.Fatalf("output type = %q, want function_call_output", output.Type)
	}
	return output.CallID, output.Output
}

func TestCatalogBuildsNonStrictResponsesFunctionDefinitions(t *testing.T) {
	catalog := newTestCatalog(t, testTool(t))
	definitions, err := catalog.ResponseTools()
	if err != nil {
		t.Fatalf("ResponseTools() error = %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("ResponseTools() count = %d, want 1", len(definitions))
	}
	var got struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	}
	if err := json.Unmarshal(definitions[0], &got); err != nil {
		t.Fatalf("Unmarshal(response tool) error = %v", err)
	}
	if got.Type != "function" || got.Name != "echo" || got.Description != "Echo one string." || got.Strict {
		t.Fatalf("response tool metadata = %+v", got)
	}
	if string(got.Parameters) != string(testTool(t).Definition.InputSchema) {
		t.Fatalf("parameters = %s, want original schema %s", got.Parameters, testTool(t).Definition.InputSchema)
	}
}

func TestCatalogValidatesNormalizesAndExecutesWithoutCoercion(t *testing.T) {
	catalog := newTestCatalog(t, testTool(t))
	output, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "call-1", "echo", `{"command":"  hello  "}`), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	callID, text := decodeOutput(t, output)
	if callID != "call-1" || !strings.Contains(text, "echo:hello") {
		t.Fatalf("function output = (%q, %q)", callID, text)
	}

	invalid, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "call-2", "echo", `{"command":5}`), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall(invalid) error = %v", err)
	}
	_, invalidText := decodeOutput(t, invalid)
	if !strings.Contains(invalidText, `"status":"invalid_input"`) {
		t.Fatalf("invalid output = %s", invalidText)
	}
	if strings.Contains(invalidText, `"command":"5"`) {
		t.Fatalf("schema validation coerced a number to a string: %s", invalidText)
	}

	extra, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "call-3", "echo", `{"command":"hello","extra":true}`), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall(extra property) error = %v", err)
	}
	_, extraText := decodeOutput(t, extra)
	if !strings.Contains(extraText, "additionalProperty") || !strings.Contains(extraText, "extra") {
		t.Fatalf("extra-property validation output = %s", extraText)
	}
}

func TestCatalogConvertsBadCallsAndExecutionFailuresToToolOutputs(t *testing.T) {
	catalog := newTestCatalog(t, testTool(t))
	cases := []struct {
		name     string
		call     json.RawMessage
		wantText string
	}{
		{name: "invalid JSON arguments", call: callJSON(t, "bad-json", "echo", `{"command":`), wantText: "tool arguments must be valid JSON"},
		{name: "non-string arguments", call: callJSON(t, "bad-type", "echo", json.RawMessage(`{"command":"hello"}`)), wantText: "function call arguments must be a JSON string"},
		{name: "unknown tool", call: callJSON(t, "unknown", "missing", `{"command":"x"}`), wantText: "unknown_tool"},
		{name: "executor error", call: callJSON(t, "execution-error", "echo", `{"command":"x"}`), wantText: "tool_execution_failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output, err := catalog.ExecuteFunctionCall(context.Background(), test.call, ExecutionContext{})
			if err != nil {
				t.Fatalf("ExecuteFunctionCall() error = %v", err)
			}
			_, text := decodeOutput(t, output)
			if !strings.Contains(text, test.wantText) {
				t.Fatalf("tool output = %s, want substring %q", text, test.wantText)
			}
			if strings.Contains(text, "private error details") {
				t.Fatalf("executor error leaked: %s", text)
			}
		})
	}
}

// A tool runs on a batch worker goroutine, where an unrecovered panic ends the
// whole process. It must come back as the same output a returned error gives.
func TestCatalogConvertsToolPanicsToToolOutputs(t *testing.T) {
	panicking := testTool(t)
	panicking.Execute = func(context.Context, map[string]any, ExecutionContext) (string, error) {
		panic("private panic details")
	}
	panickingOutput := testTool(t)
	panickingOutput.Definition.ID = "native.echo_output"
	panickingOutput.Definition.Name = "echo_output"
	panickingOutput.Execute = nil
	panickingOutput.ExecuteOutput = func(context.Context, map[string]any, ExecutionContext) (json.RawMessage, error) {
		var values map[string]int
		values["boom"] = 1
		return nil, nil
	}
	catalog := newTestCatalog(t, panicking, panickingOutput)
	for _, name := range []string{"echo", "echo_output"} {
		t.Run(name, func(t *testing.T) {
			output, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "call-panic", name, `{"command":"x"}`), ExecutionContext{})
			if err != nil {
				t.Fatalf("ExecuteFunctionCall() error = %v", err)
			}
			callID, text := decodeOutput(t, output)
			if callID != "call-panic" || !strings.Contains(text, "tool_execution_failed") {
				t.Fatalf("tool output = (%q, %s)", callID, text)
			}
			if strings.Contains(text, "private panic details") || strings.Contains(text, "nil map") {
				t.Fatalf("panic value leaked: %s", text)
			}
		})
	}
}

// The example the model sees after a rejected call must keep the key order the
// tool declared, not the alphabetical order a Go map would produce.
func TestSchemaErrorKeepsExampleKeyOrder(t *testing.T) {
	tool := testTool(t)
	tool.ExampleOperation = json.RawMessage(`{"path":"example.txt","content":"...","contentEncoding":"utf8"}`)
	rendered := SchemaError("call", "Invalid arguments for echo", []map[string]string{{"keyword": "type", "message": "m"}}, tool.ExampleOperation)
	if !strings.Contains(rendered, `example_operation: {"path":"example.txt","content":"...","contentEncoding":"utf8"}`) {
		t.Fatalf("example key order was not preserved:\n%s", rendered)
	}
	if without := SchemaError("call", "s", nil, nil); strings.Contains(without, "example_operation") {
		t.Fatalf("a missing example must be omitted:\n%s", without)
	}
}

func TestCatalogRejectsDuplicateNamesAndIdentifiesUserDecisionTools(t *testing.T) {
	first := testTool(t)
	second := testTool(t)
	second.Definition.ID = "native.other"
	if _, err := NewCatalog([]Tool{first, second}); err == nil {
		t.Fatal("NewCatalog() accepted duplicate names")
	}

	first.RequiresUserDecision = true
	catalog := newTestCatalog(t, first)
	if !catalog.IsUserDecisionCall(callJSON(t, "call-1", "echo", `{"command":"x"}`)) {
		t.Fatal("IsUserDecisionCall() = false, want true")
	}
}

func TestCatalogChangesStateTreatsOnlyReadOnlyToolsAsSafe(t *testing.T) {
	build := func(name string, readOnly bool) Tool {
		validator, err := CompileSchema([]byte(`{"type":"object"}`))
		if err != nil {
			t.Fatal(err)
		}
		return Tool{
			Definition: Definition{ID: "test:" + name, Source: "native", Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Validator:  validator,
			ReadOnly:   readOnly,
			Execute: func(context.Context, map[string]any, ExecutionContext) (string, error) {
				return "", nil
			},
		}
	}
	catalog, err := NewCatalog([]Tool{build("lookup", true), build("mutate", false)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		call string
		want bool
	}{
		{`{"name":"lookup"}`, false},
		{`{"name":"mutate"}`, true},
		{`{"name":"no_such_tool"}`, false},
		{`{}`, false},
		{`not json`, false},
	} {
		if got := catalog.ChangesState(json.RawMessage(tc.call)); got != tc.want {
			t.Errorf("ChangesState(%s) = %v, want %v", tc.call, got, tc.want)
		}
	}
	var none *Catalog
	if none.ChangesState(json.RawMessage(`{"name":"mutate"}`)) {
		t.Error("a nil catalog reports a state change")
	}
}

// A ReadOnly tool can still decide per call that a call changes files: the subagent
// tool does, for an agent that may write. That call must order like a write.
func TestCatalogChangesStateFollowsAPerCallHookOfAReadOnlyTool(t *testing.T) {
	validator, err := CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	hooked := Tool{
		Definition: Definition{ID: "test:hooked", Source: "native", Name: "hooked", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator,
		ReadOnly:   true,
		MayChangeFiles: func(arguments map[string]any) bool {
			mode, _ := arguments["mode"].(string)
			return mode == "write"
		},
		Execute: func(context.Context, map[string]any, ExecutionContext) (string, error) { return "", nil },
	}
	plain := hooked
	plain.Definition.ID, plain.Definition.Name, plain.MayChangeFiles = "test:plain", "plain", nil
	catalog, err := NewCatalog([]Tool{hooked, plain})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		call            string
		changes, hooked bool
	}{
		{`{"name":"hooked","arguments":"{\"mode\":\"write\"}"}`, true, true},
		{`{"name":"hooked","arguments":"{\"mode\":\"read\"}"}`, false, false},
		{`{"name":"hooked","arguments":"{}"}`, false, false},
		{`{"name":"hooked","arguments":"not json"}`, false, false},
		{`{"name":"hooked"}`, false, false},
		{`{"name":"plain","arguments":"{\"mode\":\"write\"}"}`, false, false},
		{`{"name":"missing","arguments":"{\"mode\":\"write\"}"}`, false, false},
	} {
		if got := catalog.ChangesState(json.RawMessage(tc.call)); got != tc.changes {
			t.Errorf("ChangesState(%s) = %v, want %v", tc.call, got, tc.changes)
		}
		if got := catalog.CallMayChangeFiles(json.RawMessage(tc.call)); got != tc.hooked {
			t.Errorf("CallMayChangeFiles(%s) = %v, want %v", tc.call, got, tc.hooked)
		}
	}
	var none *Catalog
	if none.CallMayChangeFiles(json.RawMessage(`{"name":"hooked"}`)) {
		t.Error("a nil catalog reported a change")
	}
}
