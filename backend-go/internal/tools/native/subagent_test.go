package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
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
	return sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: "/tmp/unused", Agents: agents}
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
	host := newTestAsyncHost(t)
	manager := NewSubagentManager()
	t.Cleanup(func() { _ = manager.Close() })
	tool, err := newSubagentToolAsync(settings, parentTools, nil, launcher, nil, manager, nil, host)
	if err != nil {
		t.Fatalf("newSubagentToolAsync: %v", err)
	}
	return catalogWithAsync(t, host, parentTools, tool), tool
}

// testSessionKey is the session the test helpers call as unless a test names another.
const testSessionKey = "test"

func newTestAsyncHost(t *testing.T) *AsyncHost {
	t.Helper()
	host, err := NewAsyncHost(filepath.Join(t.TempDir(), ".async"), asyncmgr.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	return host
}

// catalogWithAsync returns a catalog of the parent tools, the subagent tool and the async
// tool over host, the way a session with tools.native.async has it.
func catalogWithAsync(t *testing.T, host *AsyncHost, parent []tools.Tool, subagent tools.Tool) *tools.Catalog {
	t.Helper()
	asyncTool, err := NewAsyncTool(host)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(append([]tools.Tool(nil), parent...), subagent, asyncTool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// asyncCall calls the async tool of catalog as the default test session.
func asyncCall(t *testing.T, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	return asyncCallAs(t, catalog, testSessionKey, arguments)
}

func asyncCallAs(t *testing.T, catalog *tools.Catalog, sessionKey, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_a", "name": "async", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: sessionKey})
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

// asyncWait waits for the next completion signal of the default test session.
func asyncWait(t *testing.T, catalog *tools.Catalog) string {
	t.Helper()
	return asyncCall(t, catalog, `{"action":"wait","wait_ms":3000}`)
}

func subagentCall(t *testing.T, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	return subagentCallWith(t, context.Background(), catalog, arguments)
}

func subagentCallWith(t *testing.T, ctx context.Context, catalog *tools.Catalog, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "call_9", "name": "subagent", "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(ctx, call, tools.ExecutionContext{WorkspaceRoot: "/work", SessionDir: "/sess", SessionKey: testSessionKey})
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
	if strings.Join(toolNames(catalog), ",") != "read,write,bash,lookup,grep,subagent,async" {
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
		"subagent":     {[]string{"subagent"}, `lists "subagent", but tools.native.subagent.max_depth is 1`},
		"async":        {[]string{"read", "async"}, `lists "async", but the session does not enable tools.native.async`},
		"tool_smith":   {[]string{"tool_smith"}, `lists "tool_smith", but the session does not enable tools.native.tool_smith`},
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
		"extra key":   `{"task":"x","timeout":5}`,
		"task number": `{"task":7}`,
		"not json":    `{`,
	} {
		output := subagentCall(t, catalog, arguments)
		if !strings.Contains(output, "error_type: schema") && !strings.Contains(output, "validation_failed") {
			t.Errorf("%s was not rejected by the schema:\n%s", name, output)
		}
	}
	// A missing, empty or blank task is not an error: it asks for the agent list.
	for name, arguments := range map[string]string{"missing": `{}`, "empty": `{"task":""}`, "blank": `{"task":"   "}`} {
		if output := subagentCall(t, catalog, arguments); !strings.Contains(output, "2 agents available") {
			t.Errorf("%s task did not list the agents:\n%s", name, output)
		}
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

// mcpStub is a loaded MCP tool: the ID is what an agent's tools list writes.
func mcpStub(t *testing.T, server, name, approval string) tools.Tool {
	t.Helper()
	tool := withExecutors(t, tools.Tool{Definition: tools.Definition{
		ID: "mcp:" + server + ":" + name, Source: "mcp", Server: server, Name: "mcp_" + server + "_" + name + "_0123456789", Description: name,
	}, Approval: approval})[0]
	return tool
}

// buildWithMCP builds the subagent tool the way the resolver does: first the tool,
// then Bind with the MCP tools that were loaded afterwards.
func buildWithMCP(t *testing.T, settings sessionfs.NativeSubagentSettings, launcher SubagentLauncher, mcpTools []tools.Tool, parent ...tools.Tool) (*tools.Catalog, error) {
	t.Helper()
	hook := &SubagentMCP{}
	parentTools := withExecutors(t, parent...)
	tool, err := NewSubagentToolWithMCP(settings, parentTools, launcher, hook)
	if err != nil {
		t.Fatalf("NewSubagentToolWithMCP: %v", err)
	}
	if err := hook.Bind(mcpTools); err != nil {
		return nil, err
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	return catalog, nil
}

func agentWithTools(name string, names ...string) sessionfs.NativeSubagentAgent {
	agent := agentNamed(name)
	agent.Tools, agent.HasTools = names, true
	return agent
}

func TestSubagentChildGetsOnlyTheMCPToolsItsListNames(t *testing.T) {
	launcher := &fakeLauncher{}
	mcpTools := []tools.Tool{
		mcpStub(t, "zvec", "search", ""),
		mcpStub(t, "zvec", "reindex", ""),
		mcpStub(t, "browser", "click", ""),
	}
	catalog, err := buildWithMCP(t, subagentSettings(
		agentWithTools("finder", "read", "mcp:zvec:search"),
		agentWithTools("mcponly", "mcp:zvec:search", "mcp:zvec:reindex"),
		agentNamed("plain"),
	), launcher, mcpTools, readOnlyTool("read"), readOnlyTool("grep"))
	if err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]string{
		"finder":  "read,mcp_zvec_search_0123456789",
		"mcponly": "mcp_zvec_search_0123456789,mcp_zvec_reindex_0123456789",
		// No tools list: native read-only tools only, never an MCP tool.
		"plain": "read,grep",
	} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+agent+`"}`)
		var got string
		for _, request := range launcher.requests {
			if request.Agent.Name == agent {
				got = strings.Join(toolNames(request.Tools), ",")
			}
		}
		if got != want {
			t.Errorf("agent %s child tools = %q, want %q", agent, got, want)
		}
	}
}

func TestSubagentBindRefusesAnMCPToolThatIsNotLoadedOrNeedsApproval(t *testing.T) {
	loaded := []tools.Tool{mcpStub(t, "zvec", "search", ""), mcpStub(t, "gated", "lookup", tools.ApprovalAsk)}
	for name, tc := range map[string]struct{ tool, want string }{
		"unknown tool":   {"mcp:zvec:nope", `lists "mcp:zvec:nope", which is not a loaded MCP tool (loaded: mcp:gated:lookup, mcp:zvec:search)`},
		"unknown server": {"mcp:other:search", `lists "mcp:other:search", which is not a loaded MCP tool`},
		"needs approval": {"mcp:gated:lookup", `lists "mcp:gated:lookup", whose server asks for approval`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "read", tc.tool)), &fakeLauncher{}, loaded, readOnlyTool("read"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	// With no MCP tool loaded at all the message says so.
	_, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "mcp:zvec:search")), &fakeLauncher{}, nil, readOnlyTool("read"))
	if err == nil || !strings.Contains(err.Error(), "(loaded: none)") {
		t.Fatalf("error = %v", err)
	}
	// An agent that lists no MCP tool is not affected by what is loaded.
	if _, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "read")), &fakeLauncher{}, loaded, readOnlyTool("read")); err != nil {
		t.Fatalf("native-only agent: %v", err)
	}
}

func TestSubagentWildcardGivesTheChildEveryToolOfThatServerOnce(t *testing.T) {
	launcher := &fakeLauncher{}
	mcpTools := []tools.Tool{
		mcpStub(t, "zvec", "search", ""),
		mcpStub(t, "zvec", "reindex", ""),
		mcpStub(t, "zvecx", "other", ""),
		mcpStub(t, "browser", "click", ""),
	}
	catalog, err := buildWithMCP(t, subagentSettings(
		agentWithTools("wild", "read", "mcp:zvec:*"),
		// The exact entry repeats a tool the wildcard already gave: it is added once.
		agentWithTools("both", "mcp:zvec:search", "mcp:zvec:*"),
	), launcher, mcpTools, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]string{
		"wild": "read,mcp_zvec_reindex_0123456789,mcp_zvec_search_0123456789",
		"both": "mcp_zvec_search_0123456789,mcp_zvec_reindex_0123456789",
	} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+agent+`"}`)
		var got string
		for _, request := range launcher.requests {
			if request.Agent.Name == agent {
				got = strings.Join(toolNames(request.Tools), ",")
			}
		}
		if got != want {
			t.Errorf("agent %s child tools = %q, want %q (server zvecx and browser must not match)", agent, got, want)
		}
	}
}

func TestSubagentWildcardSkipsToolsOfAServerThatAsksButExactStillFails(t *testing.T) {
	loaded := []tools.Tool{
		mcpStub(t, "mixed", "open", ""),
		mcpStub(t, "mixed", "gated", tools.ApprovalAsk),
		mcpStub(t, "gated", "lookup", tools.ApprovalAsk),
	}
	launcher := &fakeLauncher{}
	catalog, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "mcp:mixed:*")), launcher, loaded, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "mcp_mixed_open_0123456789" {
		t.Errorf("child tools = %q, want only the tool that needs no approval", got)
	}
	for name, tc := range map[string]struct{ entry, want string }{
		"every tool asks": {"mcp:gated:*", `"mcp:gated:*", but server "gated" asks for approval`},
		"no tool loaded":  {"mcp:ghost:*", `"mcp:ghost:*", but server "ghost" has no loaded tool`},
		"exact asks":      {"mcp:mixed:gated", `lists "mcp:mixed:gated", whose server asks for approval`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildWithMCP(t, subagentSettings(agentWithTools("a", tc.entry)), &fakeLauncher{}, loaded, readOnlyTool("read"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func withDeny(settings sessionfs.NativeSubagentSettings, deny ...string) sessionfs.NativeSubagentSettings {
	settings.DenyTools = deny
	return settings
}

// The profile's tools.deny removes a tool from every child: from the default set, from
// an explicit list, and from a wildcard's expansion. Other tools and servers stay.
func TestSubagentProfileDenyRemovesToolsFromEveryChild(t *testing.T) {
	launcher := &fakeLauncher{}
	mcpTools := []tools.Tool{
		mcpStub(t, "zvec", "search", ""),
		mcpStub(t, "zvec", "reindex", ""),
		mcpStub(t, "zvecx", "other", ""),
	}
	catalog, err := buildWithMCP(t, withDeny(subagentSettings(
		agentNamed("plain"),
		agentWithTools("listed", "read", "grep"),
		agentWithTools("wild", "read", "mcp:zvec:*"),
		agentWithTools("exact", "mcp:zvec:search", "mcp:zvec:reindex"),
		agentWithTools("other", "mcp:zvecx:*"),
	), "grep", "mcp:zvec:reindex"), launcher, mcpTools, readOnlyTool("read"), readOnlyTool("grep"), readOnlyTool("glob"))
	if err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]string{
		"plain":  "read,glob",
		"listed": "read",
		"wild":   "read,mcp_zvec_search_0123456789",
		"exact":  "mcp_zvec_search_0123456789",
		"other":  "mcp_zvecx_other_0123456789",
	} {
		subagentCall(t, catalog, `{"task":"x","agent":"`+agent+`"}`)
		var got string
		for _, request := range launcher.requests {
			if request.Agent.Name == agent {
				got = strings.Join(toolNames(request.Tools), ",")
			}
		}
		if got != want {
			t.Errorf("agent %s child tools = %q, want %q", agent, got, want)
		}
	}
}

// A deny entry for a whole server removes the server from a child; a deny entry that
// matches no loaded tool changes nothing (the profile checks that it names a tool).
func TestSubagentProfileDenyOfAWholeServerAndOfNothing(t *testing.T) {
	launcher := &fakeLauncher{}
	mcpTools := []tools.Tool{mcpStub(t, "zvec", "search", ""), mcpStub(t, "browser", "click", "")}
	catalog, err := buildWithMCP(t, withDeny(subagentSettings(
		agentWithTools("a", "read", "mcp:zvec:search", "mcp:browser:click"),
	), "mcp:zvec:*", "mcp:ghost:*"), launcher, mcpTools, readOnlyTool("read"))
	if err != nil {
		t.Fatal(err)
	}
	subagentCall(t, catalog, `{"task":"x"}`)
	if got := strings.Join(toolNames(launcher.requests[0].Tools), ","); got != "read,mcp_browser_click_0123456789" {
		t.Errorf("child tools = %q, want the denied server gone and the other kept", got)
	}
}

// An agent left with nothing to use fails the load, and the message says the deny list
// is why, so the cause is not mistaken for a typo in the agent file.
func TestSubagentProfileDenyThatEmptiesAnAgentIsAnError(t *testing.T) {
	loaded := []tools.Tool{mcpStub(t, "zvec", "search", "")}
	for name, tc := range map[string]struct {
		agent sessionfs.NativeSubagentAgent
		deny  []string
		want  string
	}{
		"listed tools all denied":  {agentWithTools("a", "read"), []string{"read"}, "agents.a has no tool to use"},
		"default set all denied":   {agentNamed("a"), []string{"read"}, "agents.a has no tool to use"},
		"wildcard server denied":   {agentWithTools("a", "mcp:zvec:*"), []string{"mcp:zvec:*"}, "agents.a has no tool to use"},
		"exact mcp tool denied":    {agentWithTools("a", "mcp:zvec:search"), []string{"mcp:zvec:search"}, "agents.a has no tool to use"},
		"message names the reason": {agentWithTools("a", "read"), []string{"read"}, "the profile's tools.deny is in effect: read"},
	} {
		t.Run(name, func(t *testing.T) {
			hook := &SubagentMCP{}
			_, err := NewSubagentToolWithMCP(withDeny(subagentSettings(tc.agent), tc.deny...), withExecutors(t, readOnlyTool("read")), &fakeLauncher{}, hook)
			if err == nil {
				err = hook.Bind(loaded)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// Without a deny list the same agent loads, so the error comes from the deny.
	if _, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "read")), &fakeLauncher{}, loaded, readOnlyTool("read")); err != nil {
		t.Fatalf("no deny: %v", err)
	}
}

// With allow_write on, a denied change tool is not available to a child either.
func TestSubagentProfileDenyAlsoRemovesAChangeTool(t *testing.T) {
	settings := withDeny(subagentSettings(agentWithTools("a", "read", "write")), "write")
	settings.AllowWrite = true
	parentTools := withExecutors(t, readOnlyTool("read"), writeTool("write"))
	_, err := NewSubagentTool(settings, parentTools, &fakeLauncher{})
	if err != nil {
		t.Fatalf("a denied change tool must be dropped from the list, not fail: %v", err)
	}
	settings.DenyTools = nil
	tool, err := NewSubagentTool(settings, parentTools, &fakeLauncher{})
	if err != nil {
		t.Fatal(err)
	}
	if !tool.MayChangeFiles(map[string]any{"task": "x", "agent": "a"}) {
		t.Fatal("control: without a deny list the agent that lists write may change files")
	}
	settings.DenyTools = []string{"write"}
	tool, err = NewSubagentTool(settings, parentTools, &fakeLauncher{})
	if err != nil {
		t.Fatal(err)
	}
	if tool.MayChangeFiles(map[string]any{"task": "x", "agent": "a"}) {
		t.Fatal("with write denied the agent must no longer count as a file changer")
	}
}

func TestSubagentWithMCPToolsAnswersUnavailableUntilBound(t *testing.T) {
	launcher := &fakeLauncher{}
	hook := &SubagentMCP{}
	parentTools := withExecutors(t, readOnlyTool("read"))
	tool, err := NewSubagentToolWithMCP(subagentSettings(agentWithTools("a", "mcp:zvec:search")), parentTools, launcher, hook)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog(append(parentTools, tool))
	if err != nil {
		t.Fatal(err)
	}
	output := subagentCall(t, catalog, `{"task":"x"}`)
	if !strings.Contains(output, "The MCP tools of agent a were not loaded") || len(launcher.requests) != 0 {
		t.Fatalf("output = %s, launcher calls = %d", output, len(launcher.requests))
	}
}

func TestSubagentMCPHookIsSafeWithoutASubagentTool(t *testing.T) {
	var nilHook *SubagentMCP
	if err := nilHook.Bind([]tools.Tool{mcpStub(t, "zvec", "search", "")}); err != nil {
		t.Fatal(err)
	}
	if err := (&SubagentMCP{}).Bind(nil); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentNativeNamesStillRejectAnMCPLookingNameThatIsNotMCP(t *testing.T) {
	// "mcp_x" has no colon form, so it is an unknown native name and the error points
	// at the right spelling.
	_, err := NewSubagentTool(subagentSettings(agentWithTools("a", "mcp_x")), withExecutors(t, readOnlyTool("read")), nil)
	if err == nil || !strings.Contains(err.Error(), "an MCP tool is written mcp:<server>:<tool>") {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentBindIgnoresAToolThatIsNotFromMCP(t *testing.T) {
	// Only tools whose source is "mcp" can be listed as mcp:<server>:<tool>. A native
	// tool with such an ID, handed to Bind by mistake, must not become available.
	impostor := withExecutors(t, tools.Tool{Definition: tools.Definition{ID: "mcp:zvec:search", Source: "native", Name: "impostor", Description: "x"}, ReadOnly: true})[0]
	_, err := buildWithMCP(t, subagentSettings(agentWithTools("a", "read", "mcp:zvec:search")), &fakeLauncher{}, []tools.Tool{impostor}, readOnlyTool("read"))
	if err == nil || !strings.Contains(err.Error(), `"mcp:zvec:search", which is not a loaded MCP tool (loaded: none)`) {
		t.Fatalf("error = %v", err)
	}
}

func describedAgent(name, description string) sessionfs.NativeSubagentAgent {
	agent := agentNamed(name)
	agent.Description = description
	return agent
}

func TestSubagentAgentListShrinksWithItsBudgetAtTheByteBoundaries(t *testing.T) {
	// Long descriptions, so the names text is shorter than the full text.
	long := strings.Repeat("d", 100)
	agents := []sessionfs.NativeSubagentAgent{describedAgent("alpha", long), describedAgent("beta", long), agentNamed("gamma")}
	full := "\nAgents:\n- alpha: " + long + "\n- beta: " + long + "\n- gamma"
	names := "\nAgents: alpha, beta, gamma. Call without a task to list them with descriptions."
	count := "\n3 agents configured. Call without a task to list them with descriptions."
	if !(len(count) < len(names) && len(names) < len(full)) {
		t.Fatalf("test setup: count %d, names %d, full %d are not increasing", len(count), len(names), len(full))
	}

	for name, tc := range map[string]struct {
		budget int64
		want   string
	}{
		"full fits exactly":     {int64(len(full)), full},
		"full is one byte over": {int64(len(full)) - 1, names},
		"names fit exactly":     {int64(len(names)), names},
		"names one byte over":   {int64(len(names)) - 1, count},
		"count always sent":     {1, count},
		"zero never lists":      {0, count},
		"large budget":          {1 << 20, full},
	} {
		t.Run(name, func(t *testing.T) {
			if got := subagentAgentList(agents, tc.budget, nil); got != tc.want {
				t.Fatalf("budget %d:\n got %q\nwant %q", tc.budget, got, tc.want)
			}
		})
	}
	if got := subagentAgentList(agents[:1], 0, nil); got != "\n1 agent configured. Call without a task to list them with descriptions." {
		t.Fatalf("one agent, no budget = %q", got)
	}
	if got := subagentAgentList(agents[:1], 4096, nil); got != "\nAgents:\n- alpha: "+long {
		t.Fatalf("one agent = %q", got)
	}
	// With short descriptions the full text is shorter than the names text, so the
	// full text is kept whenever it fits; the names tier is never larger than needed.
	short := []sessionfs.NativeSubagentAgent{describedAgent("a", "x"), describedAgent("b", "y")}
	if got := subagentAgentList(short, 4096, nil); got != "\nAgents:\n- a: x\n- b: y" {
		t.Fatalf("short list = %q", got)
	}
}

func manyAgents(count int) []sessionfs.NativeSubagentAgent {
	agents := make([]sessionfs.NativeSubagentAgent, 0, count)
	for index := 0; index < count; index++ {
		agents = append(agents, describedAgent(fmt.Sprintf("agent-%03d", index), "Searches one area of the code and reports file paths and line numbers."))
	}
	return agents
}

func TestSubagentDescriptionTierFollowsTheNumberOfAgentsAtTheDefaultBudget(t *testing.T) {
	descriptionFor := func(count int) string {
		settings := subagentSettings(manyAgents(count)...)
		settings.ListMaxBytes = 4096
		_, tool := buildSubagentCatalog(t, settings, &fakeLauncher{}, readOnlyTool("read"))
		return tool.Definition.Description
	}
	// 40 lines of about 90 bytes fit in 4096: the full list.
	if d := descriptionFor(40); !strings.Contains(d, "- agent-039: Searches one area") {
		t.Fatalf("40 agents are not listed in full:\n%s", d)
	}
	// 100 agents do not fit in full (about 9 KB) but their names do (about 1 KB).
	d := descriptionFor(100)
	if strings.Contains(d, "Searches one area") || !strings.Contains(d, "\nAgents: agent-000, agent-001, ") || !strings.Contains(d, "agent-099. Call without a task") {
		t.Fatalf("100 agents did not fall back to names:\n%s", d)
	}
	if len(d) > 1800 {
		t.Fatalf("100 agents cost %d bytes of description, expected about 1 KB of names", len(d))
	}
	// 600 names do not fit in 4096 either: a count and the way to ask.
	d = descriptionFor(600)
	if strings.Contains(d, "agent-000") || !strings.Contains(d, "600 agents configured. Call without a task") {
		t.Fatalf("600 agents did not collapse to a count:\n%s", d)
	}
	if len(d) > len(subagentDescription)+100 {
		t.Fatalf("600 agents cost %d bytes, more than the base text plus a short note", len(d))
	}
}

func TestSubagentCallWithoutATaskListsEveryAgentWithItsDescription(t *testing.T) {
	launcher := &fakeLauncher{}
	var many []sessionfs.NativeSubagentAgent
	for _, name := range []string{"one", "two", "three"} {
		many = append(many, describedAgent(name, "Does "+name+".\nSecond line."))
	}
	catalog, _ := buildSubagentCatalog(t, subagentSettings(many...), launcher, readOnlyTool("read"))
	output := subagentCall(t, catalog, `{}`)
	for _, want := range []string{`"status":"ok"`, "3 agents available", "agents: 3", "one: Does one. Second line.", "three: Does three. Second line."} {
		if !strings.Contains(output, want) {
			t.Errorf("list result lacks %q:\n%s", want, output)
		}
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("listing started %d children", len(launcher.requests))
	}
	// With an agent named, the task still runs as before.
	subagentCall(t, catalog, `{"task":"do it","agent":"two"}`)
	if len(launcher.requests) != 1 || launcher.requests[0].Agent.Name != "two" {
		t.Fatalf("requests = %+v", launcher.requests)
	}
}

// When every agent is denied the tool says so instead of telling the model to create one.
func TestSubagentWithAllAgentsDeniedSaysTheyAreDisabled(t *testing.T) {
	launcher := &fakeLauncher{}
	settings := subagentSettings()
	settings.ConfiguredAgentDirs = []string{"/proj/.jad/subagents"}
	settings.DenyAgents = []string{"secret"}
	catalog, tool := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"))
	for name, text := range map[string]string{
		"description": tool.Definition.Description,
		"list":        subagentCall(t, catalog, `{}`),
		"task":        subagentCall(t, catalog, `{"task":"do it","agent":"secret"}`),
	} {
		if !strings.Contains(text, "tools.native.subagent.deny_agents") || strings.Contains(text, "create a folder") {
			t.Errorf("%s:\n%s", name, text)
		}
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("a denied agent started %d children", len(launcher.requests))
	}
}

// With no agent the tool still loads. The list, the description and a task all say there
// is none and where to create one, and nothing is started.
func TestSubagentWithZeroAgentsExplainsHowToCreateOne(t *testing.T) {
	launcher := &fakeLauncher{}
	settings := subagentSettings()
	settings.ConfiguredAgentDirs = []string{"/proj/.jad/subagents", "/shared/agents"}
	catalog, tool := buildSubagentCatalog(t, settings, launcher, readOnlyTool("read"))
	for _, want := range []string{"No agents are configured", "/proj/.jad/subagents or /shared/agents", "agent.json", "next message"} {
		if !strings.Contains(tool.Definition.Description, want) {
			t.Errorf("description lacks %q:\n%s", want, tool.Definition.Description)
		}
	}
	list := subagentCall(t, catalog, `{}`)
	for _, want := range []string{`"status":"ok"`, "0 agents available", "No agents are configured", "/proj/.jad/subagents"} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
	task := subagentCall(t, catalog, `{"task":"do it"}`)
	for _, want := range []string{"unknown_agent", "No agents are configured", "/shared/agents"} {
		if !strings.Contains(task, want) {
			t.Errorf("task result lacks %q:\n%s", want, task)
		}
	}
	if len(launcher.requests) != 0 {
		t.Fatalf("a task with no agent started %d children", len(launcher.requests))
	}
	// A named agent gets the same answer, not "No agent named".
	if named := subagentCall(t, catalog, `{"task":"x","agent":"ghost"}`); !strings.Contains(named, "No agents are configured") {
		t.Fatalf("named agent with none configured:\n%s", named)
	}
}

func TestSubagentWithZeroAgentsAndNoFolderSaysToDefineOneInline(t *testing.T) {
	_, tool := buildSubagentCatalog(t, subagentSettings(), &fakeLauncher{}, readOnlyTool("read"))
	if !strings.Contains(tool.Definition.Description, "define one in tools.native.subagent.agents") {
		t.Fatalf("description = %s", tool.Definition.Description)
	}
}

func TestSubagentListWorksWhereChildrenCannotRun(t *testing.T) {
	// No launcher: a task answers "unavailable", but the list needs no launcher.
	catalog, _ := buildSubagentCatalog(t, subagentSettings(describedAgent("a", "Does a.")), nil, readOnlyTool("read"))
	if output := subagentCall(t, catalog, `{}`); !strings.Contains(output, "1 agent available") || !strings.Contains(output, "a: Does a.") {
		t.Fatalf("list without a launcher:\n%s", output)
	}
	if output := subagentCall(t, catalog, `{"task":"x"}`); !strings.Contains(output, "subagent_unavailable") {
		t.Fatalf("task without a launcher:\n%s", output)
	}
}

func TestSubagentListIsCutAtTheResultLimit(t *testing.T) {
	settings := subagentSettings(describedAgent("alpha", strings.Repeat("d", 100)), describedAgent("beta", strings.Repeat("e", 100)))
	settings.ResultMaxBytes = 60
	catalog, _ := buildSubagentCatalog(t, settings, &fakeLauncher{}, readOnlyTool("read"))
	if output := subagentCall(t, catalog, `{}`); !strings.Contains(output, "truncated: true") {
		t.Fatalf("a long list was not marked truncated:\n%s", output)
	}
}
