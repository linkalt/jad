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

// The shipped schema is flat (no oneOf or anyOf: the Anthropic adapter strips them from the top level) and
// takes names (describe), run (execute one tool) and arguments (for run), nothing else. No maxItems: the
// batch limit is enforced at runtime from the profile.
func TestLoadToolShippedSchemaTakesNamesRunAndArguments(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "load_tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []struct {
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	schema := document.Tools[0].InputSchema
	for _, keyword := range []string{"oneOf", "anyOf", "allOf", "not", "if", "required"} {
		if _, ok := schema[keyword]; ok {
			t.Errorf("the schema has a top-level %s, which a provider may strip or refuse", keyword)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) != 3 {
		t.Errorf("properties = %v, want names, run and arguments", properties)
	}
	names, _ := properties["names"].(map[string]any)
	if names["type"] != "array" || names["uniqueItems"] != true || names["minItems"] != float64(1) {
		t.Errorf("names = %v, want an array with minItems 1 and uniqueItems", names)
	}
	description, _ := names["description"].(string)
	for _, want := range []string{"mcp:<server>:*", "tools.lazy_load_max", "Describe"} {
		if !strings.Contains(description, want) {
			t.Errorf("names description %q does not mention %q", description, want)
		}
	}
	if _, ok := names["maxItems"]; ok {
		t.Error("the static schema carries maxItems; the limit belongs to tools.lazy_load_max")
	}
	if run, _ := properties["run"].(map[string]any); run["type"] != "string" {
		t.Errorf("run = %v, want a string", run)
	}
	if arguments, _ := properties["arguments"].(map[string]any); arguments["type"] != "object" {
		t.Errorf("arguments = %v, want an object", arguments)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
}

// runLoadTool runs the tool's executor as the catalog does and returns the text of its result.
func runLoadTool(t *testing.T, tool tools.Tool, arguments map[string]any, execution tools.ExecutionContext) string {
	t.Helper()
	raw, err := tool.ExecuteOutput(context.Background(), arguments, execution)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("output %s is not a text: %v", raw, err)
	}
	return text
}

// A describe batch over the limit is refused with the configured limit named, and nothing is described.
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
	output := runLoadTool(t, tool, map[string]any{"names": []any{"*"}}, tools.ExecutionContext{CallID: "call-1"})
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

// A call that still sends the old name key is refused by the validator, and so is arguments that is not
// an object and a key the schema does not have.
func TestLoadToolValidatorRefusesWhatTheSchemaDoesNotHave(t *testing.T) {
	tool, err := NewLoadToolTool(newTestLazySetWithTool(t), 16)
	if err != nil {
		t.Fatal(err)
	}
	for name, arguments := range map[string]string{
		"old name key":       `{"name":"read"}`,
		"arguments a string": `{"run":"read","arguments":"x"}`,
		"run not a string":   `{"run":1}`,
		"empty run":          `{"run":""}`,
		"unknown key":        `{"run":"read","extra":1}`,
		"empty names":        `{"names":[]}`,
		"arguments an array": `{"run":"read","arguments":[]}`,
		"names not an array": `{"names":"read"}`,
	} {
		issues, err := tool.Validator.Validate([]byte(arguments))
		if err != nil {
			t.Fatal(err)
		}
		if len(issues) == 0 {
			t.Errorf("%s: %s passed the validator", name, arguments)
		}
	}
	for name, arguments := range map[string]string{"list": `{}`, "describe": `{"names":["read"]}`, "run": `{"run":"read"}`, "run with arguments": `{"run":"read","arguments":{"path":"a"}}`} {
		if issues, _ := tool.Validator.Validate([]byte(arguments)); len(issues) != 0 {
			t.Errorf("%s: %s was refused: %v", name, arguments, issues)
		}
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

// With no names and no run load_tool lists the lazy tools and says how to describe and run them; with
// names it describes them (name, id, description, inputSchema). Neither writes the lazy set.
func TestLoadToolListsAndDescribes(t *testing.T) {
	set := newTestLazySetWithTool(t)
	tool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	list := runLoadTool(t, tool, map[string]any{}, tools.ExecutionContext{CallID: "call-1"})
	if want := "read — read a file.\n\n" + loadToolUsage; list != want {
		t.Fatalf("list = %q, want %q", list, want)
	}
	described := runLoadTool(t, tool, map[string]any{"names": []any{"read", "nothing", "*"}}, tools.ExecutionContext{CallID: "call-2"})
	for _, want := range []string{"name: read\nid: native.read\ndescription: read a file.\ninputSchema: {\"type\":\"object\"}", "nothing: not a lazy tool of this profile", "read: already described above", loadToolUsage} {
		if !strings.Contains(described, want) {
			t.Errorf("describe = %q, missing %q", described, want)
		}
	}
	if strings.Contains(described, "Loaded") || set.Loaded("native.read") {
		t.Error("a describe loaded a tool or said so")
	}
	if none := runLoadTool(t, tool, map[string]any{"names": []any{"bash", "grep"}}, tools.ExecutionContext{CallID: "call-3"}); strings.Contains(none, "name: ") || strings.Contains(none, loadToolUsage) {
		t.Errorf("a describe that matched nothing = %q, want only the no-match lines", none)
	}
}

// A list and a describe leave the lazy set's state file byte for byte as it was.
func TestLoadToolListAndDescribeWriteNothing(t *testing.T) {
	dir := t.TempDir()
	set, err := tools.OpenLazySet(dir, []string{"read"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, _ := tools.CompileSchema([]byte(`{"type":"object"}`))
	set.Bind([]tools.Tool{{
		Definition: tools.Definition{ID: "native.read", Source: "native", Name: "read", Description: "read a file.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator,
		Execute:    func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "", nil },
	}})
	tool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "lazy_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	runLoadTool(t, tool, map[string]any{}, tools.ExecutionContext{CallID: "1"})
	runLoadTool(t, tool, map[string]any{"names": []any{"*"}}, tools.ExecutionContext{CallID: "2"})
	after, err := os.ReadFile(filepath.Join(dir, "lazy_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("lazy_tools.json changed:\n%s\nthen\n%s", before, after)
	}
}

// load_tool is always listed once enabled: no Listed rule, ReadOnly, and never a leaf. Enabling the key is
// the only switch.
func TestLoadToolIsAlwaysListedReadOnlyAndNoLeaf(t *testing.T) {
	tool, err := NewLoadToolTool(newTestLazySetWithTool(t), 16)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Listed != nil {
		t.Fatal("load_tool has a Listed rule; it must be listed whenever it is enabled")
	}
	if !tool.ReadOnly || tool.LeafBudget || tool.Delegate == nil {
		t.Fatalf("ReadOnly=%v LeafBudget=%v Delegate set=%v, want a read-only runner that is no leaf", tool.ReadOnly, tool.LeafBudget, tool.Delegate != nil)
	}
}

// Delegate is a pure parse of the arguments: only run without names runs anything.
func TestLoadToolDelegateParsesOnlyARun(t *testing.T) {
	for name, tc := range map[string]struct {
		arguments map[string]any
		target    string
		ok        bool
	}{
		"list":                    {map[string]any{}, "", false},
		"describe":                {map[string]any{"names": []any{"read"}}, "", false},
		"run":                     {map[string]any{"run": "read"}, "read", true},
		"run by id":               {map[string]any{"run": "mcp:docs:fetch", "arguments": map[string]any{"a": 1}}, "mcp:docs:fetch", true},
		"run and names":           {map[string]any{"run": "read", "names": []any{"read"}}, "", false},
		"arguments not an object": {map[string]any{"run": "read", "arguments": "x"}, "", false},
		"arguments alone":         {map[string]any{"arguments": map[string]any{}}, "", false},
	} {
		target, arguments, ok := loadToolDelegate(tc.arguments)
		if target != tc.target || ok != tc.ok || (ok && arguments == nil) {
			t.Errorf("%s: Delegate = %q %v %v, want %q %v", name, target, arguments, ok, tc.target, tc.ok)
		}
	}
}
