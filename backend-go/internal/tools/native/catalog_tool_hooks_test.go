package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// hookedCatalogSettings builds a catalog that lists read_image, subagent and profile_message, with a
// pre_tool hook on every tool. The hook's marker is one line per call; a blocking hook is
// added when block is set.
func hookedCatalogFixture(t *testing.T, launcher SubagentLauncher, marker string, block bool) *tools.Catalog {
	t.Helper()
	pre := eventHook("log", "*", "echo fired >> "+marker)
	if block {
		pre = blockingHook("gate", "subagent", `echo no-subagents; exit 3`)
	}
	readerAgent := agentNamed("explore")
	readerAgent.Tools, readerAgent.HasTools = []string{"read_image"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{readerAgent}}
	settings := sessionfs.SessionConfig{
		SettingPath:          filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:  []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")},
		NativeReadImage:      &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:       &subagent,
		NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024},
		Profile:              "main",
		Profiles:             []string{"main", "reviewer"},
		Hooks:                eventSettings([]sessionfs.ToolEventHook{pre}, nil),
	}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithMessenger(settings, nil, nil, nil, launcher, nil, nil, nil, messenger)
	if err != nil {
		t.Fatalf("BuildCatalogWithMessenger: %v", err)
	}
	return catalog
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func executeNamedCall(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{SessionKey: testSessionKey})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// executeNamedCallViaRunner is executeNamedCall for a call a runner dispatches: the execution context
// has ViaRunner, which is what lets a lazy tool run.
func executeNamedCallViaRunner(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": name, "arguments": arguments})
	raw, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{SessionKey: testSessionKey, ViaRunner: true})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// executeThroughLoadTool calls name through load_tool run, with the given arguments object.
func executeThroughLoadTool(t *testing.T, catalog *tools.Catalog, name, arguments string) string {
	t.Helper()
	encoded, _ := json.Marshal(map[string]any{"run": name, "arguments": json.RawMessage(arguments)})
	return executeNamedCall(t, catalog, "load_tool", string(encoded))
}

// The pre_tool hook fires for the subagent call and for the profile_message call; each one
// reports to the marker once.
func TestPreToolHookFiresForSubagentAndProfileMessageCalls(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fires")
	launcher := &fakeLauncher{}
	catalog := hookedCatalogFixture(t, launcher, marker, false)
	executeNamedCall(t, catalog, "subagent", `{"task":"x"}`)
	executeNamedCall(t, catalog, "profile_message", `{"profile":"reviewer","message":"hi"}`)
	if got := countLines(t, marker); got != 2 {
		t.Fatalf("pre_tool fired %d times for a subagent and a profile_message call, want 2", got)
	}
}

// A blocking pre_tool on subagent stops the run: the launcher never starts the child.
func TestBlockingPreToolStopsASubagentRun(t *testing.T) {
	launcher := &fakeLauncher{}
	catalog := hookedCatalogFixture(t, launcher, filepath.Join(t.TempDir(), "fires"), true)
	output := executeNamedCall(t, catalog, "subagent", `{"task":"x"}`)
	if len(launcher.requests) != 0 {
		t.Fatalf("the child ran %d times although a blocking pre_tool hook failed", len(launcher.requests))
	}
	if !strings.Contains(output, "blocked_by_hook") || !strings.Contains(output, "no-subagents") {
		t.Fatalf("output = %s, want the blocked result with the hook's report", output)
	}
}

// A native lazy tool is gated for a child too: the child's copy of read is the gated one, so a child
// calling it before a load is refused with tool_not_loaded, and the real schema is not run.
func TestChildOfAParentWithANativeLazyToolGetsTheGatedCopy(t *testing.T) {
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read_image"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	readerAgent := agentNamed("explore")
	readerAgent.Tools, readerAgent.HasTools = []string{"read_image"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 1, Agents: []sessionfs.NativeSubagentAgent{readerAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:         filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths: []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json")},
		NativeReadImage:     &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:      &subagent,
		Profile:             "main",
	}
	launcher := &fakeLauncher{}
	catalog, err := BuildCatalogWithAsync(config, nil, nil, nil, launcher, nil, nil, nil, nil, nil, set)
	if err != nil {
		t.Fatalf("BuildCatalogWithAsync: %v", err)
	}
	subagentCall(t, catalog, `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	childRead := executeNamedCall(t, launcher.requests[0].Tools, "read_image", `{"path":"notes.png"}`)
	if !strings.Contains(childRead, "tool_not_loaded") {
		t.Fatalf("the child's read of an unloaded lazy tool was not refused: %s", childRead)
	}
}

// NotLazyBindable names exactly load_tool: the one native tool that can never be lazy. The late-built
// transcript, transcripts, profile, profile_message and subagent tools are bound by lazyLate, and an
// ordinary native tool, the extension decide_systemone and an MCP tool are bound by the lazy pass.
func TestNotLazyBindableRefusesExactlyLoadTool(t *testing.T) {
	if !NotLazyBindable(tools.Definition{ID: loadToolID, Source: "native"}) {
		t.Errorf("%s can never be lazy but NotLazyBindable accepts it", loadToolID)
	}
	for _, definition := range []tools.Definition{
		{ID: subagentToolID, Source: "native", Name: "subagent"},
		{ID: transcriptToolID, Source: "native", Name: "transcript"},
		{ID: transcriptsToolID, Source: "native", Name: "transcripts"},
		{ID: profileToolID, Source: "native", Name: "profile"},
		{ID: profileMessageToolID, Source: "native", Name: "profile_message"},
		{ID: readImageToolID, Source: "native", Name: "read_image"},
		{ID: execToolID, Source: "native", Name: "exec"},
		{ID: "decide_systemone", Source: "extension", Name: "decide_systemone"},
		{ID: "mcp:docs:search", Source: "mcp", Name: "search"},
	} {
		if NotLazyBindable(definition) {
			t.Errorf("%s can be lazy but NotLazyBindable refuses it", definition.ID)
		}
	}
}

// A child that lists profile_message and subagent gets the owner's pre_tool hook on each of them, so
// a call the child makes fires it once, as the parent's call does. The child's copies are not wrapped twice.
func TestPreToolHookFiresForACallMadeByAChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fires")
	pre := eventHook("log", "*", "echo fired >> "+marker)
	childAgent := agentNamed("explore")
	childAgent.Tools, childAgent.HasTools = []string{"read_image", "profile_message", "subagent"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	config := sessionfs.SessionConfig{
		SettingPath:          filepath.Join(t.TempDir(), "setting.json"),
		ToolDefinitionPaths:  []string{filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "read_image.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "subagent.json"), filepath.Join("..", "..", "..", "..", "sample", "tools", "native", "profile_message.json")},
		NativeReadImage:      &sessionfs.NativeReadImageSettings{MaxImageBytes: 5 << 20},
		NativeSubagent:       &subagent,
		NativeProfileMessage: &sessionfs.NativeProfileMessageSettings{TimeoutMs: 5000, ResultMaxBytes: 1024},
		Profile:              "main",
		Profiles:             []string{"main", "reviewer"},
		Hooks:                eventSettings([]sessionfs.ToolEventHook{pre}, nil),
	}
	launcher := &fakeLauncher{}
	messenger := &fakeMessenger{reply: func(context.Context) (string, error) { return "ok", nil }}
	catalog, err := BuildCatalogWithMessenger(config, nil, nil, nil, launcher, nil, nil, nil, messenger)
	if err != nil {
		t.Fatalf("BuildCatalogWithMessenger: %v", err)
	}
	executeNamedCall(t, catalog, "subagent", `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	childCatalog := launcher.requests[0].Tools
	executeNamedCall(t, childCatalog, "profile_message", `{"profile":"reviewer","message":"hi"}`)
	if got := countLines(t, marker); got != 2 {
		t.Fatalf("pre_tool fired %d times after the parent's subagent and the child's profile_message, want 2", got)
	}
	executeNamedCall(t, childCatalog, "subagent", `{"task":"y","agent":"explore"}`)
	if len(launcher.requests) != 2 {
		t.Fatalf("launcher ran %d times, want 2 (the child's nested subagent)", len(launcher.requests))
	}
	if got := countLines(t, marker); got != 3 {
		t.Fatalf("pre_tool fired %d times after the child's nested subagent, want 3", got)
	}
}

// A refused lazy call never runs the real tool, and its post_tool hook still fires once: the refusal
// is a normal result, so the hook sees it like any other call.
func TestRefusedLazyCallRunsNoToolAndStillFiresPostTool(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "post")
	post := eventHook("log", "*", "echo fired >> "+marker)
	set, err := tools.OpenLazySet(t.TempDir(), []string{"read_image"}, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	real := tools.Tool{
		Definition: tools.Definition{ID: readImageToolID, Source: "native", Name: "read_image", Description: "read_image", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Validator:  validator,
		Execute: func(context.Context, map[string]any, tools.ExecutionContext) (string, error) {
			ran++
			return "the secret", nil
		},
	}
	set.Bind([]tools.Tool{real})
	gated, ok := set.Gate(real)
	if !ok {
		t.Fatal("the set did not gate the lazy tool")
	}
	hooked := WithToolEventHooks(gated, eventSettings(nil, []sessionfs.ToolEventHook{post}), "")
	catalog, err := tools.NewCatalog([]tools.Tool{hooked})
	if err != nil {
		t.Fatal(err)
	}
	output := executeNamedCall(t, catalog, "read_image", `{"path":"notes.png"}`)
	if !strings.Contains(output, "tool_not_loaded") {
		t.Fatalf("output = %s, want the refusal", output)
	}
	if ran != 0 {
		t.Fatalf("the real tool ran %d times for a refused call, want 0", ran)
	}
	if got := countLines(t, marker); got != 1 {
		t.Fatalf("post_tool fired %d times for a refused call, want 1", got)
	}
}

// The nested copy is hooked when the subagent tool is built, so a child catalog that is bound and given
// the owner's lazy set afterwards still fires the hook. There is no later call that can leave it raw.
func TestNestedCopyIsHookedBeforeAnyBindOrLazySet(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fires")
	pre := eventHook("log", "*", "echo fired >> "+marker)
	childAgent := agentNamed("explore")
	childAgent.Tools, childAgent.HasTools = []string{"read_image", "subagent"}, true
	subagent := sessionfs.NativeSubagentSettings{ResultMaxBytes: 1024, MaxConcurrent: 2, ListMaxBytes: 4096, KeepFinished: 10, MaxQueued: 4, StorageDir: t.TempDir(), MaxDepth: 2, Agents: []sessionfs.NativeSubagentAgent{childAgent}}
	validator, err := tools.CompileSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	readTool := readOnlyTool("read_image")
	readTool.Validator = validator
	readTool.Execute = func(context.Context, map[string]any, tools.ExecutionContext) (string, error) { return "ok", nil }
	mcp := NewSubagentMCP(eventSettings([]sessionfs.ToolEventHook{pre}, nil), "")
	launcher := &fakeLauncher{}
	parent, err := newSubagentTool(subagent, []tools.Tool{readTool}, nil, launcher, mcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.BindToolsets(nil, nil); err != nil {
		t.Fatal(err)
	}
	set, err := tools.OpenLazySet(t.TempDir(), nil, 256, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.SetLazy(set, nil); err != nil {
		t.Fatal(err)
	}
	// The catalog wraps the parent's copy once, as BuildCatalogWithAsync does.
	catalog, err := tools.NewCatalog([]tools.Tool{WithToolEventHooks(parent, mcp.hooks, mcp.apiKeyEnv)})
	if err != nil {
		t.Fatal(err)
	}
	executeNamedCall(t, catalog, "subagent", `{"task":"x","agent":"explore"}`)
	if len(launcher.requests) != 1 {
		t.Fatalf("launcher ran %d times, want 1", len(launcher.requests))
	}
	executeNamedCall(t, launcher.requests[0].Tools, "subagent", `{"task":"y","agent":"explore"}`)
	if got := countLines(t, marker); got != 2 {
		t.Fatalf("pre_tool fired %d times for the parent's and the nested subagent call, want 2", got)
	}
}
