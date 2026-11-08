package native

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const lookupName = "mcp_docs_lookup_0123456789"

// seenCall is what a lazy tool of runnerFixture saw of the call that reached it.
type seenCall struct {
	mu        sync.Mutex
	execution tools.ExecutionContext
	arguments map[string]any
	count     int
}

func (s *seenCall) record(arguments map[string]any, execution tools.ExecutionContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution, s.arguments, s.count = execution, arguments, s.count+1
}

// runnerFixture is the catalog of a profile with lazy tools: load_tool, one eager tool and these lazy
// tools gated by one set: an MCP tool whose result is a content array, a read-only native tool, a native
// write and a leaf. hooks wrap every tool as the catalog build does, after the gate.
type runnerFixture struct {
	catalog *tools.Catalog
	set     *tools.LazySet
	dir     string
	lookup  *seenCall
}

func newRunnerFixture(t *testing.T, hooks sessionfs.HooksSettings) runnerFixture {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	lookup := &seenCall{}
	text := func(string) func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
		return func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ran", nil }
	}
	lazy := []tools.Tool{
		{
			Definition: tools.Definition{ID: "mcp:docs:lookup", Source: "mcp", Server: "docs", Name: lookupName, Description: "looks a word up\nsecond line", InputSchema: schema},
			Validator:  validator,
			ExecuteOutput: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
				lookup.record(arguments, execution)
				return json.RawMessage(`[{"type":"input_text","text":"found"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]`), nil
			},
		},
		{Definition: tools.Definition{ID: "extension.peek", Source: "extension", Name: "peek", Description: "peeks.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Validator: mustSchema(t, `{"type":"object"}`), ReadOnly: true, Execute: text("peek")},
		{Definition: tools.Definition{ID: "extension.writer", Source: "extension", Name: "writer", Description: "writes.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Validator: mustSchema(t, `{"type":"object"}`), Execute: text("writer")},
		{Definition: tools.Definition{ID: "extension.leaf", Source: "extension", Name: "leaf", Description: "leafs.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Validator: mustSchema(t, `{"type":"object"}`), ReadOnly: true, LeafBudget: true, Execute: text("leaf")},
	}
	dir := t.TempDir()
	set := tools.NewLazySet([]string{"mcp:docs:lookup", "peek", "writer", "leaf"}, 256, nil)
	set.Bind(lazy)
	loadTool, err := NewLoadToolTool(set, 16)
	if err != nil {
		t.Fatal(err)
	}
	registered := []tools.Tool{WithToolEventHooks(loadTool, hooks, ""), withExecutors(t, readOnlyTool("read"))[0]}
	for _, tool := range lazy {
		gated, ok := set.Gate(tool)
		if !ok {
			t.Fatalf("the set did not gate %s", tool.Definition.Name)
		}
		registered = append(registered, WithToolEventHooks(gated, hooks, ""))
	}
	catalog, err := tools.NewCatalog(registered)
	if err != nil {
		t.Fatal(err)
	}
	return runnerFixture{catalog: catalog, set: set, dir: dir, lookup: lookup}
}

func mustSchema(t *testing.T, schema string) *tools.SchemaValidator {
	t.Helper()
	validator, err := tools.CompileSchema([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

// callEnvelope runs one call through the catalog with the given execution context and decodes the
// function_call_output it answers with.
func callEnvelope(t *testing.T, catalog *tools.Catalog, name, arguments string, execution tools.ExecutionContext) (callID string, output json.RawMessage) {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "outer-1", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, execution)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.CallID, envelope.Output
}

func outputText(t *testing.T, output json.RawMessage) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(output, &text); err != nil {
		return string(output)
	}
	return text
}

// run returns the target's own output value under the outer call_id, runs it with the run's execution
// context as it is (not ForNested) plus ViaRunner and the executing catalog, and by name or by ID.
func TestLoadToolRunReturnsTheTargetsOwnOutputThroughTheExecutingCatalog(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	execution := tools.ExecutionContext{SessionKey: testSessionKey, WorkspaceRoot: "/work", AgentDepth: 2, RootRun: "sub_1", RemoteHops: 1, PlanMode: true, BatchWaiting: true}
	for _, target := range []string{"mcp:docs:lookup", lookupName} {
		callID, output := callEnvelope(t, f.catalog, "load_tool", `{"run":"`+target+`","arguments":{"word":"x"}}`, execution)
		if callID != "outer-1" {
			t.Errorf("run %s: call_id = %q, want the outer call's", target, callID)
		}
		if want := `[{"type":"input_text","text":"found"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]`; string(output) != want {
			t.Errorf("run %s: output = %s, want the target's own content array", target, output)
		}
		seen := f.lookup.execution
		if !seen.ViaRunner || seen.Dispatcher == nil || seen.CallID != "outer-1" || seen.ToolName != lookupName {
			t.Errorf("run %s: the target saw %+v, want ViaRunner, a dispatcher, the outer call_id and its own name", target, seen)
		}
		if seen.WorkspaceRoot != "/work" || seen.AgentDepth != 2 || seen.RootRun != "sub_1" || seen.RemoteHops != 1 || !seen.PlanMode || !seen.BatchWaiting || seen.SessionKey != testSessionKey {
			t.Errorf("run %s: the execution context was not passed through unchanged: %+v", target, seen)
		}
		if f.lookup.arguments["word"] != "x" {
			t.Errorf("run %s: arguments = %v", target, f.lookup.arguments)
		}
	}
	if f.lookup.count != 2 {
		t.Errorf("the target ran %d times, want 2", f.lookup.count)
	}
}

// A run without arguments sends {}, so the target's own schema answers, visibly; a text result of a native tool is passed on.
func TestLoadToolRunKeepsTheTargetsSchemaErrorAndTextResult(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	_, invalid := callEnvelope(t, f.catalog, "load_tool", `{"run":"mcp:docs:lookup"}`, tools.ExecutionContext{})
	if text := outputText(t, invalid); !strings.Contains(text, "invalid_input") || !strings.Contains(text, "validation_failed") || !strings.Contains(text, "Invalid arguments for "+lookupName) {
		t.Fatalf("a run with arguments the target rejects = %s, want the target's own validation error", text)
	}
	if f.lookup.count != 0 {
		t.Fatal("the target ran with arguments its schema rejects")
	}
	if _, output := callEnvelope(t, f.catalog, "load_tool", `{"run":"peek"}`, tools.ExecutionContext{}); outputText(t, output) != "ran" {
		t.Fatalf("a run of a text tool = %s, want its own text", output)
	}
}

// Runs the runner refuses before any dispatch: a pattern, a tool that is not lazy, names together with run,
// arguments without run. Each is a result the model sees.
func TestLoadToolRunRefusesWhatIsNotOneExactLazyTool(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	for name, tc := range map[string]struct{ arguments, want string }{
		"star":             {`{"run":"*"}`, "not the pattern *"},
		"server glob":      {`{"run":"mcp:docs:*"}`, "not the pattern mcp:docs:*"},
		"an eager tool":    {`{"run":"read"}`, "unknown_lazy_tool"},
		"load_tool itself": {`{"run":"load_tool"}`, "unknown_lazy_tool"},
		"an unknown tool":  {`{"run":"nothing"}`, "unknown_lazy_tool"},
		"run and names":    {`{"run":"peek","names":["peek"]}`, "cannot be combined"},
		"arguments alone":  {`{"arguments":{}}`, "only used with run"},
	} {
		_, output := callEnvelope(t, f.catalog, "load_tool", tc.arguments, tools.ExecutionContext{})
		if text := outputText(t, output); !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want %q", name, text, tc.want)
		}
	}
	if f.lookup.count != 0 {
		t.Fatal("a refused run reached a tool")
	}
	// run and names is a model-visible invalid_input.
	_, output := callEnvelope(t, f.catalog, "load_tool", `{"run":"peek","names":["peek"]}`, tools.ExecutionContext{})
	if text := outputText(t, output); !strings.Contains(text, "invalid_input") {
		t.Errorf("run with names = %s, want status invalid_input", text)
	}
}

// A call that names a lazy tool is refused with the pointer at load_tool run; the same call through load_tool runs.
func TestLazyToolDirectCallIsRefusedWithAPointerToLoadToolRun(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	_, output := callEnvelope(t, f.catalog, "peek", `{}`, tools.ExecutionContext{})
	if text := outputText(t, output); !strings.Contains(text, "lazy_tool_not_loaded") || !strings.Contains(text, "load_tool with run") {
		t.Fatalf("a direct call = %s, want the refusal naming load_tool run", text)
	}
	_, output = callEnvelope(t, f.catalog, lookupName, `{"word":"x"}`, tools.ExecutionContext{})
	if text := outputText(t, output); !strings.Contains(text, "lazy_tool_not_loaded") || f.lookup.count != 0 {
		t.Fatalf("a direct call of the MCP tool = %s (ran %d)", text, f.lookup.count)
	}
}

// The tools array is the same bytes before and after a list, a describe and a run: a lazy tool is never
// listed and nothing load_tool does changes the array. This is the prompt-cache prefix of the tools.
func TestToolsArrayIsByteIdenticalAcrossListDescribeAndRun(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	before, err := f.catalog.ResponseTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("the array holds %d tools, want load_tool and read", len(before))
	}
	for _, raw := range before {
		for _, lazy := range []string{lookupName, `"peek"`, `"writer"`, `"leaf"`} {
			if strings.Contains(string(raw), lazy) {
				t.Fatalf("the tools array holds the lazy tool %s: %s", lazy, raw)
			}
		}
	}
	for _, step := range []struct{ name, arguments string }{
		{"load_tool", `{}`},
		{"load_tool", `{"names":["mcp:docs:lookup","peek"]}`},
		{"load_tool", `{"run":"mcp:docs:lookup","arguments":{"word":"x"}}`},
		{"load_tool", `{"names":["*"]}`},
		{"peek", `{}`},
	} {
		callEnvelope(t, f.catalog, step.name, step.arguments, tools.ExecutionContext{})
		after, err := f.catalog.ResponseTools()
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatalf("after %s %s the array holds %d tools, want %d", step.name, step.arguments, len(after), len(before))
		}
		for index := range before {
			if !bytes.Equal(before[index], after[index]) {
				t.Fatalf("after %s %s tool %d changed:\n%s\n%s", step.name, step.arguments, index, before[index], after[index])
			}
		}
	}
}

// List and describe name only the lazy tools the executing catalog holds, and a run reaches only those: a
// plan-mode catalog drops what can change files and a narrowed catalog drops what it does not name, so
// neither leaks a schema it cannot call. The wide catalog still lists all.
func TestLoadToolSeesOnlyWhatTheExecutingCatalogHolds(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	plan, err := f.catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	only, err := f.catalog.Only([]string{"load_tool", "peek"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		catalog       *tools.Catalog
		listed        []string
		missingTarget string
	}{
		"wide":      {f.catalog, []string{"peek", "writer", "leaf", lookupName}, ""},
		"plan mode": {plan, []string{"peek", "leaf"}, "writer"},
		"only":      {only, []string{"peek"}, "leaf"},
	} {
		_, list := callEnvelope(t, tc.catalog, "load_tool", `{}`, tools.ExecutionContext{})
		_, describe := callEnvelope(t, tc.catalog, "load_tool", `{"names":["*"]}`, tools.ExecutionContext{})
		for _, tool := range []string{"peek", "writer", "leaf", lookupName} {
			want := false
			for _, listed := range tc.listed {
				want = want || listed == tool
			}
			if got := strings.Contains(outputText(t, list), tool+" — "); got != want {
				t.Errorf("%s: list shows %s = %v, want %v:\n%s", name, tool, got, want, outputText(t, list))
			}
			if got := strings.Contains(outputText(t, describe), "name: "+tool+"\n"); got != want {
				t.Errorf("%s: describe shows %s = %v, want %v", name, tool, got, want)
			}
		}
		if tc.missingTarget != "" {
			_, output := callEnvelope(t, tc.catalog, "load_tool", `{"run":"`+tc.missingTarget+`"}`, tools.ExecutionContext{})
			if text := outputText(t, output); !strings.Contains(text, "unknown_tool") {
				t.Errorf("%s: run %s = %s, want unknown_tool", name, tc.missingTarget, text)
			}
		}
	}
	// A plan-mode run of a read-only lazy tool works and says it is a plan-mode call.
	_, output := callEnvelope(t, plan, "load_tool", `{"run":"peek"}`, tools.ExecutionContext{})
	if outputText(t, output) != "ran" {
		t.Errorf("a plan-mode run of a read-only lazy tool = %s", output)
	}
	// Narrowed to load_tool alone: nothing to list, nothing to run.
	none, err := f.catalog.Only([]string{"load_tool"})
	if err != nil {
		t.Fatal(err)
	}
	_, output = callEnvelope(t, none, "load_tool", `{}`, tools.ExecutionContext{})
	if outputText(t, output) != "No lazy tools." {
		t.Errorf("list in a catalog with no lazy tool = %s", output)
	}
}

// A leaf target is judged a leaf, and the runner is not: load_tool holds no LeafBudget, so its list and
// describe take no token and a run of a leaf takes the target's.
func TestLoadToolRunOfALeafIsALeafCallButLoadToolIsNot(t *testing.T) {
	f := newRunnerFixture(t, sessionfs.HooksSettings{})
	call := func(arguments string) json.RawMessage {
		encoded, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "load_tool", "arguments": arguments})
		return encoded
	}
	if !f.catalog.IsLeafCall(call(`{"run":"leaf"}`)) || f.catalog.IsLeafCall(call(`{"run":"peek"}`)) || f.catalog.IsLeafCall(call(`{}`)) || f.catalog.IsLeafCall(call(`{"names":["leaf"]}`)) {
		t.Fatal("IsLeafCall must be true only for a run of the leaf")
	}
	if !f.catalog.ChangesState(call(`{"run":"writer"}`)) || !f.catalog.ChangesState(call(`{"run":"mcp:docs:lookup"}`)) || f.catalog.ChangesState(call(`{"run":"peek"}`)) || f.catalog.ChangesState(call(`{}`)) {
		t.Fatal("ChangesState must follow the target of a run")
	}
	if got := f.catalog.SourceOf(call(`{"run":"mcp:docs:lookup"}`)); got != "mcp" {
		t.Fatalf("SourceOf(run mcp) = %q", got)
	}
}

// Hooks fire once per call. A list and a describe fire load_tool's own; a run fires the target's, once, at
// the nested dispatch, and not load_tool's; a direct call of a lazy tool fires the target's once.
func TestLoadToolHooksFireOncePerCallAndForARunThoseOfTheTarget(t *testing.T) {
	dir := t.TempDir()
	loadMarker, targetMarker := filepath.Join(dir, "load"), filepath.Join(dir, "target")
	pre := []sessionfs.ToolEventHook{
		eventHook("load", "load_tool", "echo x >> "+loadMarker),
		eventHook("target", "mcp:docs:lookup", "echo x >> "+targetMarker),
	}
	post := []sessionfs.ToolEventHook{
		eventHook("loadpost", "load_tool", "echo x >> "+loadMarker),
		eventHook("targetpost", "mcp:docs:lookup", "echo x >> "+targetMarker),
	}
	f := newRunnerFixture(t, eventSettings(pre, post))
	counts := func() (int, int) { return countLines(t, loadMarker), countLines(t, targetMarker) }
	for _, step := range []struct {
		name, name2, arguments string
		load, target           int
	}{
		{"list", "load_tool", `{}`, 2, 0},
		{"describe", "load_tool", `{"names":["mcp:docs:lookup"]}`, 4, 0},
		{"run", "load_tool", `{"run":"mcp:docs:lookup","arguments":{"word":"x"}}`, 4, 2},
		{"direct", lookupName, `{"word":"x"}`, 4, 4},
		{"names with run", "load_tool", `{"run":"mcp:docs:lookup","names":["peek"]}`, 6, 4},
	} {
		callEnvelope(t, f.catalog, step.name2, step.arguments, tools.ExecutionContext{})
		if load, target := counts(); load != step.load || target != step.target {
			t.Fatalf("after %s: load_tool hooks fired %d lines, target hooks %d; want %d and %d", step.name, load, target, step.load, step.target)
		}
	}
}

// A hook's report is appended to the text a list or a describe returns, as it is to the text of any
// tool, and the post_tool hook is told the text, not its JSON encoding.
func TestLoadToolHookReportReachesTheModelAndThePostHookGetsTheText(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	pre := []sessionfs.ToolEventHook{eventHook("say", "load_tool", "echo hello-from-hook")}
	post := []sessionfs.ToolEventHook{eventHook("keep", "load_tool", "cat > "+payload)}
	f := newRunnerFixture(t, eventSettings(pre, post))
	_, output := callEnvelope(t, f.catalog, "load_tool", `{}`, tools.ExecutionContext{})
	text := outputText(t, output)
	if !strings.Contains(text, "hello-from-hook") || !strings.Contains(text, "peek — peeks.") {
		t.Fatalf("a list with a hook = %q, want the list and the hook's report", text)
	}
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `peek — peeks.`) || strings.Contains(string(data), `\"peek`) {
		t.Fatalf("the post_tool payload = %s, want the list text", data)
	}
}
