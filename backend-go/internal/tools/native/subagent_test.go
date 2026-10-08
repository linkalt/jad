package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// fakeLauncher records the requests it gets and answers with a scripted result.
type fakeLauncher struct {
	mu       sync.Mutex
	requests []SubagentRequest
	run      func(ctx context.Context, request SubagentRequest) (SubagentResult, error)
}

func (f *fakeLauncher) Run(ctx context.Context, request SubagentRequest) (SubagentResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, request)
	}
	return SubagentResult{Text: "found it", Rounds: 2, Tokens: 1234}, nil
}

func readOnlyTool(name string) tools.Tool {
	return tools.Tool{
		Definition: tools.Definition{ID: "native." + name, Source: "native", Name: name, Description: name},
		ReadOnly:   true,
	}
}

func writeTool(name string) tools.Tool {
	return tools.Tool{Definition: tools.Definition{ID: "native." + name, Source: "native", Name: name, Description: name}}
}

func mcpReadOnlyTool(name string) tools.Tool {
	return tools.Tool{Definition: tools.Definition{ID: "mcp:srv:" + name, Source: "mcp", Server: "srv", Name: name, Description: name}, ReadOnly: true}
}

func subagentSettings(agents ...sessionfs.NativeSubagentAgent) sessionfs.NativeSubagentSettings {
	return sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, StorageDir: "/tmp/unused", Agents: agents}
}

func agentNamed(name string) sessionfs.NativeSubagentAgent {
	return sessionfs.NativeSubagentAgent{Name: name, MaxToolRounds: 5, TimeoutMS: 5000}
}

// withExecutors gives a stub tool an executor and a schema so a catalog accepts it.
func withExecutors(t *testing.T, list ...tools.Tool) []tools.Tool {
	t.Helper()
	validator, err := tools.CompileSchema(json.RawMessage(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]tools.Tool, 0, len(list))
	for _, tool := range list {
		tool.Validator = validator
		tool.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil }
		out = append(out, tool)
	}
	return out
}

// buildSubagentCatalog returns a catalog holding the parent tools and the subagent tool.
func buildSubagentCatalog(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, parent ...tools.Tool) (*tools.Catalog, tools.Tool) {
	t.Helper()
	parentTools := withExecutors(t, parent...)
	tool, err := NewSubagentTool(settings, parentTools, launcher)
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, tool
}

func subagentCall(t *testing.T, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	return subagentCallWith(t, context.Background(), catalog, arguments)
}

func subagentCallWith(t *testing.T, ctx context.Context, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(ctx, call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess"})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	return item.Output
}

func toolNames(catalog *tools.Catalog) []string {
	var names []string
	for _, definition := range catalog.Definitions() {
		names = append(names, definition.Name)
	}
	return names
}

func TestSubagentSmallestCallRunsTheOnlyAgentAndRendersTheAnswer(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"), readOnlyTool("grep"))
	output := subagentCall(t, catalog, `{"task":"find the retry delay"}`)

	for _, want := range []string{`"status":"ok"`, "Agent explore finished", "agent: explore", "rounds: 2", "tokens: 1234", "found it"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "truncated") {
		t.Errorf("a short answer was reported as cut:\n%s", output)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher got %d requests, want 1", len(launcher.requests))
	}
	request := launcher.requests[0]
	if request.Task != "find the retry delay" || request.CallID != "call_9" || request.Agent.Name != "explore" {
		t.Fatalf("request = %+v", request)
	}
	if request.Execution.WorkspaceRoot != "/work" || request.Execution.SessionDir != "/sess" {
		t.Fatalf("the child did not get the parent's roots: %+v", request.Execution)
	}
}

func TestSubagentChildGetsOnlyReadOnlyNativeToolsAndNeverItself(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher,
		readOnlyTool("read"), writeTool("write"), writeTool("bash"), mcpReadOnlyTool("lookup"), readOnlyTool("grep"))
	subagentCall(t, catalog, `{"task":"x"}`)

	got := toolNames(launcher.requests[0].Tools)
	if strings.Join(got, ",") != "read,grep" {
		t.Fatalf("child tools = %v, want read and grep only (no write, bash, MCP tool or subagent)", got)
	}
	if strings.Join(toolNames(catalog), ",") != "read,write,bash,lookup,grep,subagent" {
		t.Fatalf("parent catalog changed: %v", toolNames(catalog))
	}
}

func TestSubagentToolsListNarrowsAndRejectsAnUnavailableName(t *testing.T) {
	narrowed := agentNamed("reader")
	narrowed.Tools, narrowed.HasTools = []string{"grep"}, true
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(narrowed), launcher, readOnlyTool("read"), readOnlyTool("grep"))
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "grep" {
		t.Fatalf("narrowed child tools = %q, want grep", got)
	}

	for name, tc := range map[string]struct {
		tools []string
		want  string
	}{
		"write tool":   {[]string{"write"}, `lists "write", which is not an enabled read-only native tool (enabled: grep, read)`},
		"unknown tool": {[]string{"nope"}, `lists "nope"`},
		"mcp tool":     {[]string{"lookup"}, `lists "lookup"`},
		"subagent":     {[]string{"subagent"}, `lists "subagent"`},
	} {
		t.Run(name, func(t *testing.T) {
			bad := agentNamed("bad")
			bad.Tools, bad.HasTools = tc.tools, true
			_, err := NewSubagentTool(subagentSettings(bad), withExecutors(t, readOnlyTool("read"), readOnlyTool("grep"), writeTool("write"), mcpReadOnlyTool("lookup")), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSubagentNeedsAtLeastOneReadOnlyToolToGiveAChild(t *testing.T) {
	_, err := NewSubagentTool(subagentSettings(agentNamed("explore")), withExecutors(t, writeTool("write")), nil)
	if err == nil || !strings.Contains(err.Error(), "agents.explore has no tool to use") {
		t.Fatalf("error = %v", err)
	}
	_, err = NewSubagentTool(sessionfs.NativeSubagentSettings{MaxConcurrent: 1}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "needs at least one agent") {
		t.Fatalf("error = %v", err)
	}
	settings := subagentSettings(agentNamed("a"))
	settings.MaxConcurrent = 0
	_, err = NewSubagentTool(settings, withExecutors(t, readOnlyTool("read")), nil)
	if err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentSelectsTheNamedAgentAndExplainsAMissingOrUnknownName(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("fast"), agentNamed("deep")), launcher, readOnlyTool("read"))

	output := subagentCall(t, catalog, `{"task":"x","agent":"deep"}`)
	if !strings.Contains(output, "agent: deep") || launcher.requests[0].Agent.Name != "deep" {
		t.Fatalf("named agent not used:\n%s", output)
	}
	// A blank agent is treated as absent, and then two agents make the call ambiguous.
	for _, arguments := range []string{`{"task":"x"}`, `{"task":"x","agent":"  "}`} {
		output = subagentCall(t, catalog, arguments)
		if !strings.Contains(output, "error_code: unknown_agent") || !strings.Contains(output, "Several agents are configured") || !strings.Contains(output, "available: fast, deep") {
			t.Errorf("%s did not explain the ambiguity:\n%s", arguments, output)
		}
	}
	output = subagentCall(t, catalog, `{"task":"x","agent":"slow"}`)
	if !strings.Contains(output, "error_code: unknown_agent") || !strings.Contains(output, `No agent named \"slow\"`) && !strings.Contains(output, `No agent named "slow"`) {
		t.Errorf("unknown agent not explained:\n%s", output)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("a rejected call started a child: %d requests", len(launcher.requests))
	}
}

func TestSubagentTrimsAnAgentNameAndRejectsBadArguments(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("fast"), agentNamed("deep")), launcher, readOnlyTool("read"))
	if output := subagentCall(t, catalog, `{"task":"x","agent":" deep "}`); !strings.Contains(output, "agent: deep") {
		t.Fatalf("a padded name was not trimmed:\n%s", output)
	}
	for name, arguments := range map[string]string{
		"missing task": `{}`,
		"empty task":   `{"task":""}`,
		"extra key":    `{"task":"x","timeout":5}`,
		"task number":  `{"task":7}`,
		"not json":     `{`,
	} {
		output := subagentCall(t, catalog, arguments)
		if !strings.Contains(output, "error_type: schema") && !strings.Contains(output, "validation_failed") {
			t.Errorf("%s was not rejected by the schema:\n%s", name, output)
		}
	}
	if output := subagentCall(t, catalog, `{"task":"   "}`); !strings.Contains(output, "task must not be blank") {
		t.Errorf("a blank task was run:\n%s", output)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("rejected calls started children: %d requests", len(launcher.requests))
	}
}

func TestSubagentCutsALongAnswerOnARuneBoundaryAndSaysSo(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: strings.Repeat("é", 100), Rounds: 1}, nil
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.ResultMaxBytes = 11 // an odd limit that lands inside a two-byte rune
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "truncated: true") {
		t.Fatalf("a cut answer was not flagged:\n%s", output)
	}
	if !strings.Contains(output, strings.Repeat("é", 5)) || strings.Contains(output, strings.Repeat("é", 6)) {
		t.Fatalf("answer was not cut to 5 whole runes (10 of 11 bytes):\n%s", output)
	}
	if strings.ContainsRune(output, '\uFFFD') {
		t.Fatalf("the cut split a rune:\n%s", output)
	}
}

func TestSubagentReportsAFailedChildWithABoundedReasonAndNoRawBody(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{}, errors.New("POST /v1/responses: 500 " + strings.Repeat("secret-body ", 200))
	}}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "error_code: subagent_failed") || !strings.Contains(output, "Agent explore failed: POST /v1/responses: 500") {
		t.Fatalf("failure not reported:\n%s", output)
	}
	if len(output) > 1200 {
		t.Fatalf("the failure text was not bounded: %d bytes", len(output))
	}
	if !strings.Contains(output, "...") {
		t.Fatalf("a cut reason does not end with an ellipsis:\n%s", output)
	}
}

func TestSubagentEmptyAnswerIsAnErrorWithTheUsage(t *testing.T) {
	launcher := &fakeLauncher{run: func(context.Context, SubagentRequest) (SubagentResult, error) {
		return SubagentResult{Text: " \n ", Rounds: 3, Tokens: 77}, nil
	}}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x"}`)
	for _, want := range []string{"error_code: subagent_empty_result", "rounds: 3", "tokens: 77"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
}

func TestSubagentTimeoutIsReportedAsATimeoutNotAFailure(t *testing.T) {
	agent := agentNamed("slow")
	agent.TimeoutMS = 20
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		// A watchdog, so a timeout that is never applied fails the test instead of hanging it.
		select {
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		case <-time.After(3 * time.Second):
			return SubagentResult{}, errors.New("the child's context never ended")
		}
	}}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agent), launcher, readOnlyTool("read"))
	started := time.Now()
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "error_code: subagent_timeout") || !strings.Contains(output, "did not finish within 20 ms") {
		t.Fatalf("timeout not reported:\n%s", output)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the call took %v for a 20 ms timeout", elapsed)
	}
}

func TestSubagentStoppingTheParentCancelsTheChildAndIsNotReportedAsAFailure(t *testing.T) {
	entered := make(chan struct{})
	var sawCancel atomic.Bool
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		close(entered)
		<-ctx.Done()
		sawCancel.Store(true)
		return SubagentResult{}, ctx.Err()
	}}
	catalog, tool := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"))
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		output string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		output, err := tool.Execute(ctx, map[string]any{"task": "x"}, tools.ExecutionContext{CallID: "call_1"})
		done <- outcome{output, err}
	}()
	<-entered
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled so the runner records a stop (output %q)", got.err, got.output)
		}
		if !sawCancel.Load() {
			t.Fatal("the child never saw the cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not return after the parent was cancelled")
	}
	_ = catalog
}

func TestSubagentBoundsChildrenRunningAtOnce(t *testing.T) {
	var running, peak atomic.Int64
	release := make(chan struct{})
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		running.Add(-1)
		return SubagentResult{Text: "ok", Rounds: 1}, nil
	}}
	settings := subagentSettings(agentNamed("explore")) // MaxConcurrent is 2
	catalog, _ := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"))

	var wait sync.WaitGroup
	for range 5 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			subagentCall(t, catalog, `{"task":"x"}`)
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for running.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give a third child the chance to start wrongly
	if got := running.Load(); got != 2 {
		t.Fatalf("%d children running while the limit is 2", got)
	}
	close(release)
	wait.Wait()
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak concurrency = %d, want 2", got)
	}
	if len(launcher.requests) != 5 {
		t.Fatalf("%d children ran, want all 5 to run in turn", len(launcher.requests))
	}
}

func TestSubagentWaitingForASlotEndsWhenTheParentStops(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	launcher := &fakeLauncher{run: func(ctx context.Context, _ SubagentRequest) (SubagentResult, error) {
		started <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return SubagentResult{Text: "ok", Rounds: 1}, nil
	}}
	settings := subagentSettings(agentNamed("explore"))
	settings.MaxConcurrent = 1
	_, tool := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"))
	go func() {
		_, _ = tool.Execute(context.Background(), map[string]any{"task": "first"}, tools.ExecutionContext{CallID: "a"})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{"task": "second"}, tools.ExecutionContext{CallID: "b"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call waiting for a slot ignored the stop")
	}
	close(hold)
	if len(launcher.requests) != 1 {
		t.Fatalf("the waiting call started a child after it was cancelled: %d requests", len(launcher.requests))
	}
}

func TestSubagentWithoutALauncherAnswersThatRunsAreUnavailable(t *testing.T) {
	catalog, _ := buildSubagentCatalog(t, subagentSettings(agentNamed("explore")), nil, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "error_code: subagent_unavailable") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestSubagentDefinitionListsTheAgentsAndIsMarkedReadOnly(t *testing.T) {
	described := agentNamed("explore")
	described.Description = "Searches the code and reports findings."
	_, tool := buildSubagentCatalog(t, subagentSettings(described, agentNamed("plain")), nil, readOnlyTool("read"))
	if !tool.ReadOnly || tool.RequiresUserDecision || tool.Approval != "" {
		t.Fatalf("flags = readOnly %v, userDecision %v, approval %q", tool.ReadOnly, tool.RequiresUserDecision, tool.Approval)
	}
	description := tool.Definition.Description
	if !strings.Contains(description, "\nAgents:\n- explore: Searches the code and reports findings.\n- plain") {
		t.Fatalf("description lacks the agent list:\n%s", description)
	}
	if tool.Definition.ID != subagentToolID || tool.Definition.Name != "subagent" || tool.Definition.Source != "native" {
		t.Fatalf("definition = %+v", tool.Definition)
	}
}

func TestSubagentShippedDefinitionMatchesTheInCodeSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []tools.Definition `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil || len(document.Tools) != 1 {
		t.Fatalf("manifest: %v, %d tools", err, len(document.Tools))
	}
	same, err := sameJSON(document.Tools[0].InputSchema, subagentInputSchema)
	if err != nil || !same {
		t.Fatalf("sample/tools/native/subagent.json inputSchema differs from the in-code schema (err %v)", err)
	}
	if document.Tools[0].ID != subagentToolID || document.Tools[0].Name != "subagent" || document.Tools[0].Description != subagentDescription {
		t.Fatalf("manifest definition = %+v", document.Tools[0])
	}
}

func TestSubagentChildNeverGetsASubagentToolEvenIfOneIsAmongTheParentTools(t *testing.T) {
	nested := readOnlyTool("subagent")
	nested.Definition.ID = subagentToolID
	launcher := &fakeLauncher{}
	_, tool := buildSubagentCatalogFrom(t, subagentSettings(agentNamed("explore")), launcher, readOnlyTool("read"), nested)
	if _, err := tool.Execute(context.Background(), map[string]any{"task": "x"}, tools.ExecutionContext{CallID: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "read" {
		t.Fatalf("child tools = %q, want read only: a child must not be able to start another child", got)
	}
}

// buildSubagentCatalogFrom builds only the subagent tool over the given parent tools,
// without putting it into a catalog beside them, so a parent that already holds a tool
// with the subagent id does not collide.
func buildSubagentCatalogFrom(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, parent ...tools.Tool) (*tools.Catalog, tools.Tool) {
	t.Helper()
	tool, err := NewSubagentTool(settings, withExecutors(t, parent...), launcher)
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	return nil, tool
}
