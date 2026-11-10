package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPlanModeKeepsReadOnlyToolsAndAllowEntries(t *testing.T) {
	base := testTool(t)
	reader := namedTool(t, base, "native.read", "read")
	reader.ReadOnly = true
	writer := namedTool(t, base, "native.write", "write")
	task := namedTool(t, base, "native.task", "task")
	agent := namedTool(t, base, "native.subagent", "subagent")
	agent.ReadOnly = true
	agent.MayChangeFiles = func(arguments map[string]any) bool { return arguments["command"] == "write" }
	mcpTool := namedTool(t, base, "mcp:docs:search", "mcp_docs_search")
	mcpTool.Definition.Source = "mcp"
	catalog := newTestCatalog(t, reader, writer, task, agent, mcpTool)

	planned, err := catalog.PlanMode([]string{"task", "mcp:docs:*"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range planned.Definitions() {
		names = append(names, definition.Name)
	}
	if strings.Join(names, ",") != "read,task,subagent,mcp_docs_search" {
		t.Fatalf("plan-mode tools = %v", names)
	}
	if len(catalog.Definitions()) != 5 {
		t.Fatal("PlanMode changed the receiver")
	}
	run := func(c *Catalog, id, name, command string) string {
		t.Helper()
		output, err := c.ExecuteFunctionCall(context.Background(), callJSON(t, id, name, `{"command":"`+command+`"}`), ExecutionContext{})
		if err != nil {
			t.Fatal(err)
		}
		_, text := decodeOutput(t, output)
		return text
	}
	if text := run(planned, "c1", "write", "x"); !strings.Contains(text, "unknown_tool") || strings.Contains(text, "echo:") {
		t.Errorf("write in plan mode = %s", text)
	}
	if text := run(planned, "c2", "read", "x"); !strings.Contains(text, "echo:x") {
		t.Errorf("read in plan mode = %s", text)
	}
	if text := run(planned, "c3", "subagent", "look"); !strings.Contains(text, "echo:look") {
		t.Errorf("a read-only sub-agent call in plan mode = %s", text)
	}
	text := run(planned, "c4", "subagent", "write")
	if strings.Contains(text, "echo:") || !strings.Contains(text, PlanModeErrorCode) || !strings.Contains(text, "forbidden") || !strings.Contains(text, "c4") {
		t.Errorf("a writing sub-agent call in plan mode = %s", text)
	}
	if text := run(catalog, "c5", "subagent", "write"); !strings.Contains(text, "echo:write") {
		t.Errorf("the full catalog refused a writing sub-agent call: %s", text)
	}

	// An allow entry keeps a tool whole, with no refusal.
	whole, err := catalog.PlanMode([]string{"subagent"})
	if err != nil {
		t.Fatal(err)
	}
	if text := run(whole, "c6", "subagent", "write"); !strings.Contains(text, "echo:write") {
		t.Errorf("an allowed sub-agent was refused: %s", text)
	}
	var none *Catalog
	if planned, err := none.PlanMode(nil); planned != nil || err != nil {
		t.Fatalf("nil catalog: %v %v", planned, err)
	}
}

// Only keeps the tools the entries cover, in catalog order; an entry that covers nothing
// narrows nothing more, and an empty list keeps no tool.
func TestOnlyNarrowsToTheEntries(t *testing.T) {
	base := testTool(t)
	reader := namedTool(t, base, "native.read", "read")
	writer := namedTool(t, base, "native.write", "write")
	search := namedTool(t, base, "mcp:docs:search", "mcp_docs_search")
	search.Definition.Source = "mcp"
	other := namedTool(t, base, "mcp:git:log", "mcp_git_log")
	other.Definition.Source = "mcp"
	catalog := newTestCatalog(t, reader, writer, search, other)
	names := func(c *Catalog) string {
		var names []string
		for _, definition := range c.Definitions() {
			names = append(names, definition.Name)
		}
		return strings.Join(names, ",")
	}
	for entries, want := range map[string]string{
		"write,read":       "read,write",
		"read,mcp:docs:*":  "read,mcp_docs_search",
		"grep,mcp:git:log": "mcp_git_log",
		"":                 "",
	} {
		list := []string{}
		if entries != "" {
			list = strings.Split(entries, ",")
		}
		narrowed, err := catalog.Only(list)
		if err != nil || names(narrowed) != want {
			t.Errorf("Only(%v) = %q, %v; want %q", list, names(narrowed), err, want)
		}
	}
	if names(catalog) != "read,write,mcp_docs_search,mcp_git_log" {
		t.Fatal("Only changed the receiver")
	}
}

// Every tool a plan-mode catalog keeps sees PlanMode on its calls; the full catalog's do not.
func TestPlanModeMarksTheCallsOfKeptTools(t *testing.T) {
	var seen []bool
	base := testTool(t)
	record := func(name string, readOnly bool) Tool {
		tool := namedTool(t, base, "native."+name, name)
		tool.ReadOnly = readOnly
		tool.ExecuteOutput = nil
		tool.Execute = func(_ context.Context, _ map[string]any, execution ExecutionContext) (string, error) {
			seen = append(seen, execution.PlanMode)
			return `{"status":"ok"}`, nil
		}
		return tool
	}
	catalog, err := NewCatalog([]Tool{record("look", true), record("shell", false)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanMode([]string{"shell"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"look", "shell"} {
		if _, err := plan.ExecuteFunctionCall(context.Background(), callJSON(t, "p-"+name, name, `{"command":"x"}`), ExecutionContext{}); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "w-"+name, name, `{"command":"x"}`), ExecutionContext{}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(seen, []bool{true, false, true, false}) {
		t.Fatalf("PlanMode seen = %v", seen)
	}
}

// A call of a tool plan mode guards carries ExecutionContext.PlanGuarded into nested runs
// (ForNested), so a tool that starts runs of its own can apply the same refusal below; a tool
// an allow entry keeps whole carries PlanMode only.
func TestPlanModeMarksGuardedCallsForNestedRuns(t *testing.T) {
	var seen ExecutionContext
	tool := namedTool(t, testTool(t), "native.subagent", "subagent")
	tool.ReadOnly = true
	tool.MayChangeFiles = func(map[string]any) bool { return false }
	tool.Execute = func(_ context.Context, _ map[string]any, execution ExecutionContext) (string, error) {
		seen = execution
		return "ok", nil
	}
	catalog := newTestCatalog(t, tool)
	for name, tc := range map[string]struct {
		allow         []string
		wantGuarded   bool
		wantNestedToo bool
	}{"guarded": {nil, true, true}, "allowed whole": {[]string{"subagent"}, false, false}} {
		planned, err := catalog.PlanMode(tc.allow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := planned.ExecuteFunctionCall(context.Background(), callJSON(t, "c", "subagent", `{"command":"x"}`), ExecutionContext{}); err != nil {
			t.Fatal(err)
		}
		if !seen.PlanMode || seen.PlanGuarded != tc.wantGuarded || seen.ForNested().PlanGuarded != tc.wantNestedToo {
			t.Errorf("%s: PlanMode %v PlanGuarded %v nested %v", name, seen.PlanMode, seen.PlanGuarded, seen.ForNested().PlanGuarded)
		}
	}
	seen = ExecutionContext{}
	if _, err := catalog.ExecuteFunctionCall(context.Background(), callJSON(t, "c", "subagent", `{"command":"x"}`), ExecutionContext{}); err != nil {
		t.Fatal(err)
	}
	if seen.PlanMode || seen.PlanGuarded {
		t.Error("a call outside plan mode must carry neither flag")
	}
}
