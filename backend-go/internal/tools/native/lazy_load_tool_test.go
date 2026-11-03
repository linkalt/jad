package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// The shipped load_tool definition must match the in-code tool: the same ID, name, description
// and input schema. A change to one without the other would give the model a different tool
// than the one the gate checks.
func TestLoadToolShippedDefinitionMatchesTheInCodeTool(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "load_tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	shipped := document.Tools[0]
	tool, err := NewLoadToolTool(newTestLazySet(t), 16)
	if err != nil {
		t.Fatal(err)
	}
	if shipped.ID != tool.Definition.ID || shipped.Name != tool.Definition.Name || shipped.Source != tool.Definition.Source {
		t.Fatalf("shipped %+v, in code %+v", shipped, tool.Definition)
	}
	if shipped.Description != tool.Definition.Description {
		t.Fatalf("shipped description %q, in code %q", shipped.Description, tool.Definition.Description)
	}
	same, err := sameJSON(shipped.InputSchema, tool.Definition.InputSchema)
	if err != nil || !same {
		t.Fatalf("sample/tools/native/load_tool.json inputSchema differs from the in-code schema (err %v)", err)
	}
}

// newTestLazySet opens an empty lazy set in a fresh state folder, enough for a tool that only
// needs the set to exist.
func newTestLazySet(t *testing.T) *tools.LazySet {
	t.Helper()
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// The shipped schema takes a names array and nothing else: no name alias, and no maxItems, since
// the batch limit is enforced at runtime from the profile.
func TestLoadToolShippedSchemaTakesABatchOfNames(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "load_tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []struct {
			InputSchema struct {
				Properties map[string]map[string]any `json:"properties"`
				Additional any                       `json:"additionalProperties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	schema := document.Tools[0].InputSchema
	if _, ok := schema.Properties["name"]; ok {
		t.Error("the schema still has the single name key")
	}
	names := schema.Properties["names"]
	if names["type"] != "array" || names["uniqueItems"] != true || names["minItems"] != float64(1) {
		t.Errorf("names = %v, want an array with minItems 1 and uniqueItems", names)
	}
	description, _ := names["description"].(string)
	for _, want := range []string{"mcp:<server>:*", "tools.lazy_load_max", "lazy tools"} {
		if !strings.Contains(description, want) {
			t.Errorf("names description %q does not mention %q", description, want)
		}
	}
	if _, ok := names["maxItems"]; ok {
		t.Error("the static schema carries maxItems; the limit belongs to tools.lazy_load_max")
	}
	if schema.Additional != false {
		t.Errorf("additionalProperties = %v, want false", schema.Additional)
	}
}

// A batch over the limit is refused with the configured limit named, and nothing is loaded.
func TestLoadToolRefusesABatchOverTheLimit(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read", "grep"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	set.Bind([]tools.Tool{
		{Definition: tools.Definition{ID: "native.read", Source: "native", Name: "read", Description: "read a file.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Validator: validator, Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "", nil }},
		{Definition: tools.Definition{ID: "native.grep", Source: "native", Name: "grep", Description: "grep a file.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Validator: validator, Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "", nil }},
	})
	tool, err := NewLoadToolTool(set, 1)
	if err != nil {
		t.Fatal(err)
	}
	// "*" selects read and grep; the limit of 1 counts both, so the batch is refused.
	output, err := tool.Execute(context.Background(), map[string]any{"names": []any{"*"}}, tools.ExecutionContext{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "matched 2 lazy tools, the limit is 1 (tools.lazy_load_max)") || !strings.Contains(output, "batch_too_large") {
		t.Fatalf("output = %s, want the limit refusal", output)
	}
	if set.Loaded("native.read") {
		t.Fatal("a refused batch loaded a tool")
	}
}

// Duplicate names are refused by the validator, not de-duplicated.
func TestLoadToolValidatorRefusesDuplicateNames(t *testing.T) {
	tool, err := NewLoadToolTool(newTestLazySetWithTool(t), 16)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := tool.Validator.Validate([]byte(`{"names":["read","read"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("a duplicate name passed the validator")
	}
}

// A call that still sends the old name key is refused by the validator.
func TestLoadToolValidatorRefusesTheOldNameKey(t *testing.T) {
	tool, err := NewLoadToolTool(newTestLazySetWithTool(t), 16)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := tool.Validator.Validate([]byte(`{"name":"read"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) == 0 {
		t.Fatal("the old name key passed the validator")
	}
}

// newTestLazySetWithTool opens a lazy set that binds one native tool, "read".
func newTestLazySetWithTool(t *testing.T) *tools.LazySet {
	t.Helper()
	set := newTestLazySet(t)
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	set.Bind([]tools.Tool{{
		Definition: tools.Definition{ID: "native.read", Source: "native", Name: "read", Description: "read a file.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator,
		Execute:    func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "", nil },
	}})
	return set
}

// A load says the loaded tools are listed, with their full definitions, from the next request,
// and names them with their summaries instead of repeating the definition the tools array carries.
// A second load of the same tool says it was already loaded.
func TestLoadToolResultSaysAvailableFromTheNextRequest(t *testing.T) {
	set := newTestLazySetWithTool(t)
	tool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"names": []any{"read"}}, tools.ExecutionContext{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if output != "Loaded. Available, with the full definition, from the next request.\n\nread — read a file." {
		t.Fatalf("output = %q, want the next-request sentence and the tool's summary line", output)
	}
	again, err := tool.Execute(context.Background(), map[string]any{"names": []any{"read"}}, tools.ExecutionContext{CallID: "call-2"})
	if err != nil {
		t.Fatal(err)
	}
	if again != "read — read a file. (already loaded)" {
		t.Errorf("second load = %q, want the summary marked already loaded and no sentence", again)
	}
}

// A batch that matches nothing changes nothing: no sentence, no write, and the tool stays unlisted.
func TestLoadToolBatchThatMatchesNothingChangesNothing(t *testing.T) {
	set := newTestLazySetWithTool(t)
	tool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), map[string]any{"names": []any{"bash", "grep"}}, tools.ExecutionContext{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "Available from the next request") {
		t.Errorf("output = %q, want no next-request sentence for a batch that loaded nothing", output)
	}
	if set.Loaded("native.read") {
		t.Error("a batch that matched nothing loaded a tool")
	}
}

// load_tool is always listed once enabled, even after every lazy tool is loaded, so the model can
// still list them. Enabling the key is the only switch.
func TestLoadToolIsAlwaysListedOnceEnabled(t *testing.T) {
	set := newTestLazySetWithTool(t)
	tool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Listed != nil {
		t.Fatal("load_tool has a Listed rule; it must be listed whenever it is enabled")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"names": []any{"read"}}, tools.ExecutionContext{CallID: "call-1"}); err != nil {
		t.Fatal(err)
	}
	if tool.Listed != nil {
		t.Error("load_tool gained a Listed rule after a load")
	}
}
