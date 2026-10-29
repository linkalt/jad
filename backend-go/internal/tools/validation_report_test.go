package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// tallyTool takes a list of integers, so every string in the list fails.
func tallyTool(t *testing.T) Tool {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"integer"}}},"required":["a"],"additionalProperties":false}`)
	validator, err := CompileSchema(schema)
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	return Tool{
		Definition: Definition{
			ID:          "native.tally",
			Source:      "native",
			Name:        "tally",
			Description: "Count a list of integers.",
			InputSchema: schema,
		},
		Validator:        validator,
		ExampleOperation: json.RawMessage(`{"a":[1,2]}`),
		Execute: func(context.Context, map[string]any, ExecutionContext) (string, error) {
			return "ok", nil
		},
	}
}

func tallyCall(t *testing.T, callID string, arguments map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("Marshal(arguments) error = %v", err)
	}
	return callJSON(t, callID, "tally", string(encoded))
}

func tallyArguments(count int, item any) map[string]any {
	items := make([]any, count)
	for index := range items {
		items[index] = item
	}
	return map[string]any{"a": items}
}

// A rejected call is persisted in the transcript and re-sent on every round, so
// its report must not grow with the argument list. Without the bound, 1,000 bad
// items turn 4 KB of arguments into a ~162 KB result (40x).
func TestCatalogBoundsASchemaValidationReport(t *testing.T) {
	catalog := newTestCatalog(t, tallyTool(t))
	arguments := tallyArguments(1000, "x")
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("Marshal(arguments) error = %v", err)
	}
	output, err := catalog.ExecuteFunctionCall(context.Background(), tallyCall(t, "call-many", arguments), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	_, text := decodeOutput(t, output)
	t.Logf("arguments=%d bytes report=%d bytes", len(encodedArguments), len(text))
	if !strings.Contains(text, `980 more issues omitted`) {
		t.Fatalf("report does not count the dropped issues:\n%s", text)
	}
	if len(text) > 2*len(encodedArguments) {
		t.Fatalf("report of %d bytes amplifies %d bytes of arguments", len(text), len(encodedArguments))
	}
	if count := strings.Count(text, `"keyword":"type"`); count != maxReportedValidationIssues {
		t.Fatalf("report keeps %d issues, want %d", count, maxReportedValidationIssues)
	}
}

// The full list stays the validator's contract: only the model-facing report is
// bounded, and the AJV shape of a small failure is unchanged.
func TestSchemaValidationReportKeepsEveryIssueForSmallFailures(t *testing.T) {
	validator, err := CompileSchema(tallyTool(t).Definition.InputSchema)
	if err != nil {
		t.Fatalf("CompileSchema() error = %v", err)
	}
	issues, err := validator.Validate([]byte(`{"a":["x"]}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("Validate() returned %d issues, want 1", len(issues))
	}
	bounded := boundValidationIssues(issues)
	if len(bounded) != 1 {
		t.Fatalf("boundValidationIssues() returned %d issues, want 1", len(bounded))
	}
	if bounded[0].InstancePath != "/a/0" || bounded[0].Keyword != "type" || bounded[0].Message != "must be integer" {
		t.Fatalf("issue changed: %+v", bounded[0])
	}
	big, err := validator.Validate([]byte(`{"a":["x","y","z"]}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(big) != 3 {
		t.Fatalf("Validate() returned %d issues, want 3", len(big))
	}
	if kept := boundValidationIssues(big); len(kept) != 3 || kept[2].Message == "0 more issues omitted" {
		t.Fatalf("a report below the cap changed: %+v", kept)
	}
}

// The strings inside one issue can grow on their own: the instance path names
// properties from the arguments, and an enum carries values from the schema.
func TestSchemaValidationReportCutsTheTextOfOneIssue(t *testing.T) {
	catalog := newTestCatalog(t, tallyTool(t))
	long := strings.Repeat("p", 1000)
	output, err := catalog.ExecuteFunctionCall(context.Background(), tallyCall(t, "call-wide", map[string]any{"a": []any{"x"}, long: 1}), ExecutionContext{})
	if err != nil {
		t.Fatalf("ExecuteFunctionCall() error = %v", err)
	}
	_, text := decodeOutput(t, output)
	t.Logf("report=%d bytes", len(text))
	if len(text) > 2*maxReportedValidationTextSize+1024 {
		t.Fatalf("one issue was not cut: %d bytes", len(text))
	}
	if strings.Contains(text, long) {
		t.Fatal("the report holds the whole property name")
	}
	if !strings.Contains(text, strings.Repeat("p", maxReportedValidationTextSize)+"...") {
		t.Fatalf("expected a cut property name:\n%s", text)
	}
}
