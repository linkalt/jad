package native

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// batchProbe records how the lazy tools of batchFixture ran: in what order they started and how many
// were in flight at once. A tool named in gate waits until `want` tools are in flight together, which
// proves they really ran in parallel without a sleep to hope for.
type batchProbe struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   []string
	finished  []string
	arrived   int
	want      int
	together  chan struct{}
	timedOut  bool
}

func newBatchProbe(want int) *batchProbe {
	return &batchProbe{want: want, together: make(chan struct{})}
}

// enter marks one tool in flight. With want > 1 it blocks until want tools are in flight together,
// and reports false when they never were (the batch ran them one at a time).
func (p *batchProbe) enter(name string) bool {
	p.mu.Lock()
	p.active++
	p.maxActive = max(p.maxActive, p.active)
	p.started = append(p.started, name)
	p.arrived++
	if p.want > 1 && p.arrived == p.want {
		close(p.together)
	}
	p.mu.Unlock()
	if p.want <= 1 {
		// One at a time must be observable: leave the others time to overlap if they could.
		time.Sleep(15 * time.Millisecond)
		return true
	}
	select {
	case <-p.together:
		return true
	case <-time.After(3 * time.Second):
		p.mu.Lock()
		p.timedOut = true
		p.mu.Unlock()
		return false
	}
}

func (p *batchProbe) leave(name string) {
	p.mu.Lock()
	p.active--
	p.finished = append(p.finished, name)
	p.mu.Unlock()
}

// batchFixture is a catalog with load_tool and these lazy tools: read-only peek, peek2 and peek3 (each
// answers "out-<name>"), a writer that can change state, a read-only leaf, boom that panics, and
// lookup, whose result is a content array with an image.
type batchFixture struct {
	catalog *tools.Catalog
	probe   *batchProbe
	loadMax int
}

func newBatchFixture(t *testing.T, want, loadMax int, hooks sessionfs.HooksSettings) batchFixture {
	t.Helper()
	probe := newBatchProbe(want)
	schema := json.RawMessage(`{"type":"object"}`)
	validator := mustSchema(t, `{"type":"object"}`)
	text := func(name string) func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
		return func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			probe.enter(name)
			defer probe.leave(name)
			return "out-" + name, nil
		}
	}
	extension := func(name string, readOnly, leaf bool) tools.Tool {
		return tools.Tool{Definition: tools.Definition{ID: "extension." + name, Source: "extension", Name: name, Description: name + ".", InputSchema: schema}, Validator: validator, ReadOnly: readOnly, LeafBudget: leaf, Execute: text(name)}
	}
	boom := extension("boom", true, false)
	boom.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
		panic("private details")
	}
	lookupSchema := json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`)
	lookup := tools.Tool{
		Definition: tools.Definition{ID: "mcp:docs:lookup", Source: "mcp", Server: "docs", Name: lookupName, Description: "looks a word up", InputSchema: lookupSchema},
		Validator:  mustSchema(t, string(lookupSchema)),
		ReadOnly:   true,
		ExecuteOutput: func(_ context.Context, arguments map[string]any, _ tools.ExecutionContext) (json.RawMessage, error) {
			return json.RawMessage(`[{"type":"input_text","text":"found ` + arguments["word"].(string) + `"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"auto"}]`), nil
		},
	}
	lazy := []tools.Tool{extension("peek", true, false), extension("peek2", true, false), extension("peek3", true, false), extension("writer", false, false), extension("leaf", true, true), boom, lookup}
	set := tools.NewLazySet([]string{"peek", "peek2", "peek3", "writer", "leaf", "boom", "mcp:docs:lookup"}, 256, nil)
	set.Bind(lazy)
	loadTool, err := NewLoadToolTool(set, loadMax)
	if err != nil {
		t.Fatal(err)
	}
	registered := []tools.Tool{WithToolEventHooks(loadTool, hooks, "")}
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
	return batchFixture{catalog: catalog, probe: probe, loadMax: loadMax}
}

func runsArguments(items ...string) string {
	return `{"runs":[` + strings.Join(items, ",") + `]}`
}

func runItem(target string) string { return `{"run":"` + target + `"}` }

// index returns where each text first appears in the output and fails when one is missing or the
// texts are out of order.
func requireInOrder(t *testing.T, text string, wants ...string) {
	t.Helper()
	at := 0
	for _, want := range wants {
		found := strings.Index(text[at:], want)
		if found < 0 {
			t.Fatalf("%q is missing, or out of order, in:\n%s", want, text)
		}
		at += found + len(want)
	}
}

func TestLoadToolDelegateRunsParsesOnlyAWellFormedBatch(t *testing.T) {
	for name, tc := range map[string]struct {
		arguments string
		want      []string
	}{
		"two runs":                      {`{"runs":[{"run":"a"},{"run":"b","arguments":{"x":1}}]}`, []string{"a", "b"}},
		"the same run twice":            {`{"runs":[{"run":"a"},{"run":"a","arguments":{"x":2}}]}`, []string{"a", "a"}},
		"empty list":                    {`{"runs":[]}`, nil},
		"not a list":                    {`{"runs":{"run":"a"}}`, nil},
		"an item that is not an object": {`{"runs":["a"]}`, nil},
		"an item without run":           {`{"runs":[{"arguments":{}}]}`, nil},
		"an empty run":                  {`{"runs":[{"run":""}]}`, nil},
		"a run that is no text":         {`{"runs":[{"run":1}]}`, nil},
		"arguments not object":          {`{"runs":[{"run":"a","arguments":[]}]}`, nil},
		"an extra key":                  {`{"runs":[{"run":"a","other":1}]}`, nil},
		"with run":                      {`{"runs":[{"run":"a"}],"run":"a"}`, nil},
		"with names":                    {`{"runs":[{"run":"a"}],"names":["a"]}`, nil},
		"with arguments":                {`{"runs":[{"run":"a"}],"arguments":{}}`, nil},
		"no runs":                       {`{"run":"a"}`, nil},
	} {
		arguments, err := tools.DecodeArguments([]byte(tc.arguments))
		if err != nil {
			t.Fatal(err)
		}
		targets, ok := loadToolDelegateRuns(arguments)
		if ok != (tc.want != nil) {
			t.Errorf("%s: ok = %v", name, ok)
			continue
		}
		var got []string
		for _, target := range targets {
			got = append(got, target.Target)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: targets = %v, want %v", name, got, tc.want)
		}
		// A batch is never also a single run.
		if _, _, single := loadToolDelegate(arguments); single && ok {
			t.Errorf("%s: a batch was also parsed as a single run", name)
		}
	}
}

func TestLoadToolRunsReturnsEveryOutputInTheOrderSent(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	callID, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("writer"), runItem("peek"), runItem("peek2"), runItem("peek")), tools.ExecutionContext{})
	if callID != "outer-1" {
		t.Fatalf("call_id = %q, want the outer call's", callID)
	}
	text := outputText(t, output)
	requireInOrder(t, text,
		"--- run 1 of 4: writer ---\nout-writer",
		"--- run 2 of 4: peek ---\nout-peek",
		"--- run 3 of 4: peek2 ---\nout-peek2",
		"--- run 4 of 4: peek ---\nout-peek")
	if strings.Join(f.probe.started, ",") != "writer,peek,peek2,peek" {
		t.Errorf("without a share of the bound the items ran in this order: %v, want the order sent", f.probe.started)
	}
	if f.probe.maxActive != 1 {
		t.Errorf("%d items ran at once, want one at a time when no parallelism was granted", f.probe.maxActive)
	}
}

func TestLoadToolRunsKeepsContentArraysAndImagesByteForByte(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	_, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek"), `{"run":"mcp:docs:lookup","arguments":{"word":"x"}}`, `{"run":"`+lookupName+`","arguments":{"word":"y"}}`), tools.ExecutionContext{})
	var parts []json.RawMessage
	if err := json.Unmarshal(output, &parts); err != nil {
		t.Fatalf("a batch with an image must answer a content array: %v\n%s", err, output)
	}
	want := []string{
		`{"type":"input_text","text":"--- run 1 of 3: peek ---"}`,
		`{"type":"input_text","text":"out-peek"}`,
		`{"type":"input_text","text":"--- run 2 of 3: mcp:docs:lookup ---"}`,
		`{"type":"input_text","text":"found x"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"auto"}`,
		`{"type":"input_text","text":"--- run 3 of 3: ` + lookupName + ` ---"}`,
		`{"type":"input_text","text":"found y"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"auto"}`,
	}
	if len(parts) != len(want) {
		t.Fatalf("%d parts, want %d:\n%s", len(parts), len(want), output)
	}
	for index := range want {
		if string(parts[index]) != want[index] {
			t.Errorf("part %d = %s, want %s", index, parts[index], want[index])
		}
	}
}

func TestLoadToolRunsOneFailingItemDoesNotStopTheOthers(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	_, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(
		runItem("nothing"), runItem("*"), runItem("read"), runItem("load_tool"),
		`{"run":"mcp:docs:lookup"}`, runItem("boom"), runItem("peek")), tools.ExecutionContext{})
	text := outputText(t, output)
	requireInOrder(t, text,
		"--- run 1 of 7: nothing ---", "unknown_lazy_tool",
		"--- run 2 of 7: * ---", "not the pattern *",
		"--- run 3 of 7: read ---", "unknown_lazy_tool",
		"--- run 4 of 7: load_tool ---", "unknown_lazy_tool",
		"--- run 5 of 7: mcp:docs:lookup ---", "Invalid arguments for "+lookupName, "validation_failed",
		"--- run 6 of 7: boom ---", "tool_execution_failed",
		"--- run 7 of 7: peek ---", "out-peek")
	if strings.Contains(text, "private details") {
		t.Errorf("a panic's details reached the model:\n%s", text)
	}
	if got := strings.Count(text, "outer-1"); got < 5 {
		t.Errorf("error results carry the outer call_id %d times, want one per failed item", got)
	}
}

func TestLoadToolRunsIsBoundedByLazyLoadMaxAndRunsNothingOverIt(t *testing.T) {
	f := newBatchFixture(t, 0, 3, sessionfs.HooksSettings{})
	_, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek"), runItem("peek"), runItem("peek"), runItem("peek")), tools.ExecutionContext{})
	text := outputText(t, output)
	if !strings.Contains(text, "batch_too_large") || !strings.Contains(text, "lists 4 tools, the limit is 3 (tools.lazy_load_max)") || !strings.Contains(text, "nothing was run") {
		t.Fatalf("over the limit = %s", text)
	}
	if len(f.probe.started) != 0 {
		t.Fatalf("a refused batch ran %v", f.probe.started)
	}
	_, output = callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek"), runItem("peek"), runItem("peek")), tools.ExecutionContext{})
	if text := outputText(t, output); strings.Contains(text, "batch_too_large") || strings.Count(text, "out-peek") != 3 {
		t.Fatalf("at the limit = %s, want three results", text)
	}
}

func TestLoadToolRunsCannotBeCombinedAndRunsNothingWhenMalformed(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	for name, arguments := range map[string]string{
		"with run":         `{"runs":[{"run":"peek"}],"run":"peek"}`,
		"with names":       `{"runs":[{"run":"peek"}],"names":["peek"]}`,
		"with arguments":   `{"runs":[{"run":"peek"}],"arguments":{}}`,
		"empty list":       `{"runs":[]}`,
		"item without run": `{"runs":[{"arguments":{}},{"run":"peek"}]}`,
		"extra key":        `{"runs":[{"run":"peek","name":"x"}]}`,
		"not a list":       `{"runs":"peek"}`,
	} {
		_, output := callEnvelope(t, f.catalog, "load_tool", arguments, tools.ExecutionContext{})
		if text := outputText(t, output); !strings.Contains(text, "invalid_input") {
			t.Errorf("%s: %s, want invalid_input", name, text)
		}
	}
	if len(f.probe.started) != 0 {
		t.Fatalf("a refused call ran %v", f.probe.started)
	}
}

// Read-only items run together when the call was granted parallelism, and the results stay in the
// order sent however they finish.
func TestLoadToolRunsRunsReadOnlyItemsInParallelAndKeepsTheOrder(t *testing.T) {
	f := newBatchFixture(t, 3, 16, sessionfs.HooksSettings{})
	execution := tools.ExecutionContext{NestedParallel: 3, NestedOrdered: true}
	_, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek3"), runItem("peek"), runItem("peek2")), execution)
	if f.probe.timedOut || f.probe.maxActive != 3 {
		t.Fatalf("the three read-only items did not run together (max in flight %d, timed out %v)", f.probe.maxActive, f.probe.timedOut)
	}
	requireInOrder(t, outputText(t, output), "--- run 1 of 3: peek3 ---\nout-peek3", "--- run 2 of 3: peek ---\nout-peek", "--- run 3 of 3: peek2 ---\nout-peek2")
}

func TestLoadToolRunsGrantedOneSlotRunsOneAtATimeInOrder(t *testing.T) {
	for _, parallel := range []int{0, 1, -1} {
		f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
		execution := tools.ExecutionContext{NestedParallel: parallel}
		callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek3"), runItem("peek"), runItem("peek2")), execution)
		if f.probe.maxActive != 1 || strings.Join(f.probe.started, ",") != "peek3,peek,peek2" {
			t.Errorf("parallel %d: max in flight %d, started %v; want one at a time in the order sent", parallel, f.probe.maxActive, f.probe.started)
		}
	}
}

// With ordered scheduling a batch holding an item that can change state runs one item at a time in the
// order sent, as the calls of a response do; with another scheduling the same batch runs together.
func TestLoadToolRunsFollowsTheSchedulingRuleOfAResponse(t *testing.T) {
	ordered := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	callEnvelope(t, ordered.catalog, "load_tool", runsArguments(runItem("peek"), runItem("writer"), runItem("peek2")), tools.ExecutionContext{NestedParallel: 8, NestedOrdered: true})
	if ordered.probe.maxActive != 1 || strings.Join(ordered.probe.started, ",") != "peek,writer,peek2" {
		t.Errorf("ordered with a writer: max in flight %d, started %v; want one at a time in order", ordered.probe.maxActive, ordered.probe.started)
	}
	// The same batch under another scheduling runs together: only "ordered" serializes a writer.
	parallel := newBatchFixture(t, 3, 16, sessionfs.HooksSettings{})
	callEnvelope(t, parallel.catalog, "load_tool", runsArguments(runItem("peek"), runItem("writer"), runItem("peek2")), tools.ExecutionContext{NestedParallel: 8})
	if parallel.probe.timedOut || parallel.probe.maxActive != 3 {
		t.Errorf("not ordered: max in flight %d (timed out %v); want the three together", parallel.probe.maxActive, parallel.probe.timedOut)
	}
}

// Without the catalog's judgments (a dispatcher that is not the catalog) nothing says two items are
// safe together, so the batch runs one at a time.
func TestLoadToolRunsWithoutJudgmentsRunsOneAtATime(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	bare := struct{ tools.Dispatcher }{f.catalog}
	arguments, _ := tools.DecodeArguments([]byte(runsArguments(runItem("peek"), runItem("peek2"), runItem("peek3"))))
	execution := tools.ExecutionContext{CallID: "c", Dispatcher: bare, NestedParallel: 8}
	targets, _ := loadToolDelegateRuns(arguments)
	runs := make([]lazyRun, len(targets))
	for index, target := range targets {
		call, _ := tools.FunctionCall("c", target.Target, target.Arguments)
		runs[index] = lazyRun{target: target.Target, name: target.Target, arguments: target.Arguments, call: call}
	}
	executeLazyRuns(context.Background(), runs, execution)
	if f.probe.maxActive != 1 {
		t.Fatalf("%d items ran at once without judgments", f.probe.maxActive)
	}
	for _, run := range runs {
		if len(run.output) == 0 {
			t.Errorf("%s has no output", run.target)
		}
	}
}

// A leaf item takes one token of the leaf budget, so a budget of one lets one leaf item run at a time
// even when the batch may run three together, and the batch never waits on a token it holds itself.
func TestLoadToolRunsTakesOneLeafTokenPerLeafItem(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	budget := tools.NewLeafBudget(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("leaf"), runItem("leaf"), runItem("leaf")), tools.ExecutionContext{NestedParallel: 3, NestedOrdered: true, LeafBudget: budget})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a batch of leaf items did not finish with a budget of one")
	}
	if f.probe.maxActive != 1 || len(f.probe.started) != 3 {
		t.Fatalf("max in flight %d, started %v; want three leaf items one at a time", f.probe.maxActive, f.probe.started)
	}
	// With no budget the same batch is not bounded, and the batch itself holds no token (a full budget of
	// non-leaf items is never waited on).
	free := newBatchFixture(t, 3, 16, sessionfs.HooksSettings{})
	callEnvelope(t, free.catalog, "load_tool", runsArguments(runItem("leaf"), runItem("leaf"), runItem("leaf")), tools.ExecutionContext{NestedParallel: 3})
	if free.probe.timedOut || free.probe.maxActive != 3 {
		t.Fatalf("without a budget: max in flight %d (timed out %v), want 3", free.probe.maxActive, free.probe.timedOut)
	}
	used := newBatchFixture(t, 2, 16, sessionfs.HooksSettings{})
	full := tools.NewLeafBudget(1)
	full.Acquire() // the only token is in use elsewhere
	callEnvelope(t, used.catalog, "load_tool", runsArguments(runItem("peek"), runItem("peek2")), tools.ExecutionContext{NestedParallel: 2, LeafBudget: full})
	if used.probe.timedOut || used.probe.maxActive != 2 {
		t.Fatalf("non-leaf items waited on the leaf budget: max in flight %d (timed out %v)", used.probe.maxActive, used.probe.timedOut)
	}
}

// Each item fires the hooks of its own tool once, at the nested dispatch, and load_tool's own hooks do
// not fire for the batch; an invalid batch fires load_tool's, as an invalid single call does.
func TestLoadToolRunsFiresTheHooksOfEachTargetAndNotLoadTools(t *testing.T) {
	dir := t.TempDir()
	loadMarker, peekMarker, writerMarker := filepath.Join(dir, "load"), filepath.Join(dir, "peek"), filepath.Join(dir, "writer")
	pre := []sessionfs.ToolEventHook{
		eventHook("load", "load_tool", "echo x >> "+loadMarker),
		eventHook("peek", "peek", "echo x >> "+peekMarker),
		eventHook("writer", "writer", "echo x >> "+writerMarker),
	}
	post := []sessionfs.ToolEventHook{
		eventHook("loadpost", "load_tool", "echo x >> "+loadMarker),
		eventHook("peekpost", "peek", "echo x >> "+peekMarker),
	}
	f := newBatchFixture(t, 0, 16, eventSettings(pre, post))
	callEnvelope(t, f.catalog, "load_tool", runsArguments(runItem("peek"), runItem("writer"), runItem("peek")), tools.ExecutionContext{NestedParallel: 1})
	if got := countLines(t, loadMarker); got != 0 {
		t.Errorf("load_tool hooks fired %d times for a batch, want 0", got)
	}
	if got := countLines(t, peekMarker); got != 4 {
		t.Errorf("peek hooks fired %d times, want pre and post for each of two items", got)
	}
	if got := countLines(t, writerMarker); got != 1 {
		t.Errorf("writer hooks fired %d times, want one pre_tool", got)
	}
	callEnvelope(t, f.catalog, "load_tool", `{"runs":[{"run":"peek"}],"run":"peek"}`, tools.ExecutionContext{})
	callEnvelope(t, f.catalog, "load_tool", `{"runs":[{"run":"peek"}],"names":["peek"]}`, tools.ExecutionContext{})
	if got := countLines(t, loadMarker); got != 4 {
		t.Errorf("load_tool hooks fired %d lines after two invalid batches, want pre and post of each", got)
	}
}

// A stopped run starts no further item, and the call still answers: the model sees which items did not run.
func TestLoadToolRunsAStoppedRunStartsNoItem(t *testing.T) {
	f := newBatchFixture(t, 0, 16, sessionfs.HooksSettings{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "outer-1", "name": "load_tool", "arguments": runsArguments(runItem("peek"), runItem("nothing"), runItem("peek2"))})
	raw, err := f.catalog.ExecuteFunctionCall(ctx, call, tools.ExecutionContext{NestedParallel: 4})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal(raw, &envelope)
	text := outputText(t, envelope.Output)
	if len(f.probe.started) != 0 || strings.Count(text, "not_run") != 2 || !strings.Contains(text, "unknown_lazy_tool") {
		t.Fatalf("started %v; output:\n%s\nwant two not_run items and the refusal of the unknown one", f.probe.started, text)
	}
}

func TestLoadToolRunsFiftyItemsKeepTheirOrderUnderParallelism(t *testing.T) {
	f := newBatchFixture(t, 0, 100, sessionfs.HooksSettings{})
	items := make([]string, 0, 50)
	for index := 0; index < 50; index++ {
		items = append(items, `{"run":"`+[]string{"peek", "peek2", "peek3"}[index%3]+`","arguments":{"n":`+strconv.Itoa(index)+`}}`)
	}
	_, output := callEnvelope(t, f.catalog, "load_tool", runsArguments(items...), tools.ExecutionContext{NestedParallel: 8, NestedOrdered: true})
	text := outputText(t, output)
	at := 0
	for index := 0; index < 50; index++ {
		heading := "--- run " + strconv.Itoa(index+1) + " of 50: " + []string{"peek", "peek2", "peek3"}[index%3] + " ---"
		found := strings.Index(text[at:], heading)
		if found < 0 {
			t.Fatalf("heading %d is missing or out of order", index+1)
		}
		at += found + len(heading)
	}
	if f.probe.maxActive > 8 {
		t.Fatalf("%d items ran at once, the grant was 8", f.probe.maxActive)
	}
}
