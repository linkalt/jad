package tools

import (
	"context"
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
