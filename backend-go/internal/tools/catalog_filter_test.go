package tools

import (
	"context"
	"strings"
	"testing"
)

func namedTool(t *testing.T, base Tool, id, name string) Tool {
	t.Helper()
	tool := base
	tool.Definition.ID, tool.Definition.Name = id, name
	return tool
}

func TestCatalogFilterKeepsOrderAndReportsTheRest(t *testing.T) {
	base := testTool(t)
	catalog := newTestCatalog(t,
		namedTool(t, base, "native.a", "a"),
		namedTool(t, base, "native.b", "b"),
		namedTool(t, base, "native.c", "c"),
	)
	kept, dropped, err := catalog.Filter(func(definition Definition) bool { return definition.Name != "b" })
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range kept.Definitions() {
		names = append(names, definition.Name)
	}
	if strings.Join(names, ",") != "a,c" {
		t.Errorf("kept = %v, want a,c in the original order", names)
	}
	if len(dropped) != 1 || dropped[0].Name != "b" {
		t.Errorf("dropped = %+v, want b", dropped)
	}
	// The receiver is unchanged, so its holder can still reach every tool.
	if got := len(catalog.Definitions()); got != 3 {
		t.Errorf("the original catalog has %d tools after Filter, want 3", got)
	}
}

func TestCatalogFilterRemovesTheToolFromTheModelAndFromExecution(t *testing.T) {
	base := testTool(t)
	catalog := newTestCatalog(t, namedTool(t, base, "native.a", "a"), namedTool(t, base, "native.b", "b"))
	kept, _, err := catalog.Filter(func(definition Definition) bool { return definition.Name == "a" })
	if err != nil {
		t.Fatal(err)
	}
	response, err := kept.ResponseTools()
	if err != nil || len(response) != 1 || !strings.Contains(string(response[0]), `"name":"a"`) {
		t.Fatalf("response tools = %s, err %v", response, err)
	}
	// A model that calls the hidden tool anyway gets the unknown-tool answer.
	output, err := kept.ExecuteFunctionCall(context.Background(), callJSON(t, "c1", "b", `{"command":"x"}`), ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, text := decodeOutput(t, output); !strings.Contains(text, "unknown_tool") || strings.Contains(text, "echo:") {
		t.Errorf("hidden tool call output = %s, want unknown_tool and no run", text)
	}
	// The full catalog still runs it.
	output, err = catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "c2", "b", `{"command":"x"}`), ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, text := decodeOutput(t, output); !strings.Contains(text, "echo:x") {
		t.Errorf("the full catalog did not run b: %s", text)
	}
}

func TestCatalogFilterOfNothingKeptIsAnEmptyCatalog(t *testing.T) {
	catalog := newTestCatalog(t, testTool(t))
	kept, dropped, err := catalog.Filter(func(Definition) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	response, err := kept.ResponseTools()
	if err != nil || len(response) != 0 || len(dropped) != 1 {
		t.Errorf("response %s, dropped %d, err %v", response, len(dropped), err)
	}
	var none *Catalog
	if got, rest, err := none.Filter(func(Definition) bool { return true }); got != nil || len(rest) != 0 || err != nil {
		t.Errorf("nil catalog Filter = %v, %v, %v", got, rest, err)
	}
}
