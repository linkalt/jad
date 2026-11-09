package native

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// stubPlanSandboxProbe makes every fallback sandbox start (err nil) or fail, forgets earlier
// answers, and counts the trial starts.
func stubPlanSandboxProbe(t *testing.T, err error) *int {
	t.Helper()
	calls := new(int)
	previous := planSandboxProbe
	planSandboxProbe = func(context.Context, *sessionfs.SandboxSettings, string) error {
		*calls++
		return err
	}
	planSandboxProbes.Clear()
	t.Cleanup(func() {
		planSandboxProbe = previous
		planSandboxProbes.Clear()
	})
	return calls
}

// fallbackPlanSandbox is the fallback a session without a sandbox on exec gets on Linux.
func fallbackPlanSandbox() *sessionfs.SandboxSettings {
	fallback := sessionfs.PlanFallbackSandbox(nil)
	if fallback == nil {
		fallback = sessionfs.PlanFallbackSandbox(&sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap})
	}
	return fallback
}

// execReadOnlyCatalog is exec in a session without a sandbox, whose fallback for read_only
// and plan-mode calls is said to start on this host (the probe is stubbed).
func execReadOnlyCatalog(t *testing.T, root string) (*tools.Catalog, tools.Tool) {
	t.Helper()
	stubPlanSandboxProbe(t, nil)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.PlanSandbox = &sessionfs.SandboxSettings{Tools: []string{"exec"}, Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Network: true}
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	return catalog, tool
}

func execROCall(arguments string) json.RawMessage {
	call, _ := json.Marshal(map[string]string{"type": "function_call", "call_id": "c", "name": "exec", "arguments": arguments})
	return call
}

func runExecCall(t *testing.T, catalog *tools.Catalog, arguments, root string) string {
	t.Helper()
	output, err := catalog.ExecuteFunctionCall(context.Background(), execROCall(arguments), tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

// Only a call that runs code without read_only counts as a change: that answer decides the
// ordered scheduling (one change makes the response run in order) and the plan-mode guard.
func TestExecCallChangesStateUnlessItIsReadOnlyOrAListing(t *testing.T) {
	catalog, tool := execReadOnlyCatalog(t, t.TempDir())
	if !tool.ReadOnly || tool.MayChangeFiles == nil {
		t.Fatalf("exec must be ReadOnly with a per-call hook: readOnly=%v hook=%v", tool.ReadOnly, tool.MayChangeFiles != nil)
	}
	for name, tc := range map[string]struct {
		arguments string
		want      bool
	}{
		"code":                  {`{"code":"touch x"}`, true},
		"code, read_only true":  {`{"code":"ls","read_only":true}`, false},
		"code, read_only false": {`{"code":"touch x","read_only":false}`, true},
		"helper listing":        {`{}`, false},
		"filtered listing":      {`{"filters":["read*"]}`, false},
	} {
		if got := catalog.ChangesState(execROCall(tc.arguments)); got != tc.want {
			t.Errorf("%s: ChangesState = %v, want %v", name, got, tc.want)
		}
		if got := catalog.CallMayChangeFiles(execROCall(tc.arguments)); got != tc.want {
			t.Errorf("%s: CallMayChangeFiles = %v, want %v", name, got, tc.want)
		}
	}
}

// Plan mode keeps exec without an allow entry, and every call it runs starts in the fallback
// sandbox where no sandbox covers exec: a read_only call runs, and so does one without
// read_only, which cannot write the project either (D2, step 2 of P5).
func TestPlanModeRunsEveryExecCallInTheFallbackSandbox(t *testing.T) {
	requireBwrap(t)
	root, _, _ := sandboxDirs(t)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("plan input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planSandboxProbes.Clear()
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.PlanSandbox = fallbackPlanSandbox()
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(plan); strings.Join(got, ",") != "exec" {
		t.Fatalf("plan-mode tools = %v, want exec", got)
	}
	output := runExecCall(t, plan, `{"code":"cat notes.txt","read_only":true}`, root)
	if !strings.Contains(output, "plan input") {
		t.Fatalf("read_only call in plan mode:\n%s", output)
	}
	output = runExecCall(t, plan, `{"code":"touch made.txt 2>&1; echo scratch > \"$TMPDIR/s\" && cat \"$TMPDIR/s\""}`, root)
	if strings.Contains(output, "plan_mode") || !strings.Contains(output, "Read-only file system") || !strings.Contains(output, "scratch") {
		t.Fatalf("a call without read_only in plan mode:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "made.txt")); err == nil {
		t.Fatal("a plan-mode call wrote the project")
	}
	call := execROCall(`{"code":"touch x"}`)
	if plan.ChangesState(call) || plan.CallMayChangeFiles(call) {
		t.Fatal("a confined plan-mode call is scheduled as a change")
	}
}

// Where no sandbox can hold the call, plan mode refuses every exec call that runs code, even a
// read_only one (D3), and says why; a helper listing still runs. Outside plan mode read_only
// does not count there: the call is a change, so it runs in order.
func TestPlanModeRefusesExecCodeWhereNoSandboxWorks(t *testing.T) {
	root := t.TempDir()
	calls := stubPlanSandboxProbe(t, errors.New("bwrap: No permissions to create new namespace"))
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.PlanSandbox = fallbackPlanSandbox()
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	readOnly := execROCall(`{"code":"echo ran","read_only":true}`)
	if !catalog.ChangesState(readOnly) || !catalog.CallMayChangeFiles(readOnly) {
		t.Fatal("an unenforced read_only call is scheduled as a read")
	}
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{"code":"echo ran","read_only":true}`, `{"code":"echo ran"}`} {
		output := runExecCall(t, plan, arguments, root)
		if !strings.Contains(output, "plan_mode") || !strings.Contains(output, "no bwrap or seatbelt sandbox works here") || strings.Contains(output, "ran\n") {
			t.Errorf("%s in plan mode:\n%s", arguments, output)
		}
	}
	if output := runExecCall(t, plan, `{}`, root); strings.Contains(output, "plan_mode") {
		t.Errorf("a helper listing was refused:\n%s", output)
	}
	if *calls != 1 {
		t.Errorf("the fallback was probed %d times, want once", *calls)
	}
}

// A command backend on exec is the person's own program, which jad cannot make read-only: plan
// mode refuses exec code there and names the reason, and read_only does not count.
func TestPlanModeRefusesExecCodeUnderACommandSandbox(t *testing.T) {
	root := t.TempDir()
	stubPlanSandboxProbe(t, nil)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.Sandbox = &sessionfs.SandboxSettings{Tools: []string{"exec"}, Backend: sessionfs.SandboxBackendCommand, Command: []string{"env"}, Executable: "env"}
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.ChangesState(execROCall(`{"code":"ls","read_only":true}`)) {
		t.Fatal("read_only counted under a command sandbox")
	}
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if output := runExecCall(t, plan, `{"code":"echo ran","read_only":true}`, root); !strings.Contains(output, "plan_mode") || !strings.Contains(output, "command sandbox") {
		t.Fatalf("plan mode under a command sandbox:\n%s", output)
	}
}

// The run's own environment decides at the call (a sub-agent's or a profile_message caller's
// sandbox): a run whose sandbox covers exec with a command backend is refused in plan mode
// even where the session's settings would confine the call.
func TestPlanModeChecksTheRunsOwnSandboxBeforeTheCallRuns(t *testing.T) {
	root := t.TempDir()
	catalog, _ := execReadOnlyCatalog(t, root)
	plan, err := catalog.PlanMode(nil)
	if err != nil {
		t.Fatal(err)
	}
	call := execROCall(`{"code":"echo ran","read_only":true}`)
	if plan.ChangesState(call) {
		t.Fatal("the session's settings confine the call, so it is scheduled as a read")
	}
	command := &sessionfs.SandboxSettings{Tools: []string{"exec"}, Backend: sessionfs.SandboxBackendCommand, Command: []string{"env"}, Executable: "env"}
	output, err := plan.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: t.TempDir(), Environment: &tools.Environment{Sandbox: command}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "plan_mode") || !strings.Contains(string(output), "command sandbox") {
		t.Fatalf("a run in a command sandbox:\n%s", output)
	}
}

// A run sandbox that leaves exec out does not hold its calls, so they fall back like those of
// a session without a sandbox.
func TestExecPlanModeCallFallsBackWhereTheRunSandboxLeavesExecOut(t *testing.T) {
	stubPlanSandboxProbe(t, nil)
	settings := sessionfs.NativeBashSettings{PlanSandbox: fallbackPlanSandbox()}
	hooksOnly := &tools.Environment{Sandbox: &sessionfs.SandboxSettings{Tools: []string{"hooks"}, Backend: sessionfs.SandboxBackendBwrap}}
	for name, tc := range map[string]struct {
		execution tools.ExecutionContext
		fallback  bool
	}{
		"plan mode":                     {tools.ExecutionContext{PlanMode: true}, true},
		"plan mode, sandbox without it": {tools.ExecutionContext{PlanMode: true, Environment: hooksOnly}, true},
		"not plan mode":                 {tools.ExecutionContext{}, false},
	} {
		got := childCallSandbox("exec", settings, tc.execution)
		if (got != nil) != tc.fallback || (got != nil && strings.Join(got.WritablePaths, ",") != "{tmp}") {
			t.Errorf("%s: sandbox %+v", name, got)
		}
	}
	if got := childCallSandbox("tool_smith", sessionfs.NativeBashSettings{}, tools.ExecutionContext{PlanMode: true}); got != nil {
		t.Errorf("tool_smith without a fallback got %+v", got)
	}
}

func TestExecReadOnlyCallStartsInThePlanSandbox(t *testing.T) {
	sandbox := bwrapSandbox()
	sandbox.PlanWritablePaths = []string{"{tmp}"}
	for name, tc := range map[string]struct {
		arguments map[string]any
		planMode  bool
		want      bool
	}{
		"read_only":            {map[string]any{"code": "ls", "read_only": true}, false, true},
		"no read_only":         {map[string]any{"code": "ls"}, false, false},
		"read_only false":      {map[string]any{"code": "ls", "read_only": false}, false, false},
		"plan mode stays plan": {map[string]any{"code": "ls"}, true, true},
	} {
		execution := execReadOnlyExecution(tc.arguments, tools.ExecutionContext{PlanMode: tc.planMode})
		if execution.PlanMode != tc.want {
			t.Errorf("%s: PlanMode = %v, want %v", name, execution.PlanMode, tc.want)
		}
		got := callSandbox("exec", sandbox, execution)
		if usesPlanPaths := strings.Join(got.WritablePaths, ",") == "{tmp}"; usesPlanPaths != tc.want {
			t.Errorf("%s: writable paths = %v", name, got.WritablePaths)
		}
	}
}

// Under bwrap a read_only call cannot write the project, while the same call without
// read_only can.
func TestExecReadOnlyCallCannotWriteTheProjectUnderBwrap(t *testing.T) {
	requireBwrap(t)
	root, _, _ := sandboxDirs(t)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.Sandbox = bwrapSandbox()
	settings.Child.Sandbox.PlanWritablePaths = []string{"{tmp}"}
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	output := runExecCall(t, catalog, `{"lang":"bash","code":"touch ro.txt 2>&1; echo done","read_only":true}`, root)
	if !strings.Contains(output, "Read-only file system") {
		t.Fatalf("read_only call output:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "ro.txt")); err == nil {
		t.Fatal("a read_only call wrote the project")
	}
	runExecCall(t, catalog, `{"lang":"bash","code":"touch rw.txt"}`, root)
	if _, err := os.Stat(filepath.Join(root, "rw.txt")); err != nil {
		t.Fatalf("the call without read_only did not write the project: %v", err)
	}
}

// Without a sandbox on exec, a read_only call starts in the fallback, which keeps the project
// read-only, while the same call without read_only writes it as before.
func TestExecReadOnlyCallCannotWriteTheProjectWithoutASandbox(t *testing.T) {
	requireBwrap(t)
	planSandboxProbes.Clear()
	root, _, _ := sandboxDirs(t)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.PlanSandbox = fallbackPlanSandbox()
	tool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.ChangesState(execROCall(`{"code":"ls","read_only":true}`)) {
		t.Fatal("an enforced read_only call is scheduled as a change")
	}
	output := runExecCall(t, catalog, `{"lang":"bash","code":"touch ro.txt 2>&1; echo done","read_only":true}`, root)
	if !strings.Contains(output, "Read-only file system") {
		t.Fatalf("read_only call output:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "ro.txt")); err == nil {
		t.Fatal("a read_only call wrote the project")
	}
	runExecCall(t, catalog, `{"lang":"bash","code":"touch rw.txt"}`, root)
	if _, err := os.Stat(filepath.Join(root, "rw.txt")); err != nil {
		t.Fatalf("the call without read_only did not write the project: %v", err)
	}
}

// A child has exec like a profile does; its own allow list can leave it out.
func TestSubagentWithoutAToolsListGetsExec(t *testing.T) {
	_, execTool := execReadOnlyCatalog(t, t.TempDir())
	if plain := childNamesOf(t, agentNamed("plain"), nil, readOnlyTool("lsp"), execTool); strings.Join(plain, ",") != "lsp,exec" {
		t.Fatalf("a child without a tools list got %v", plain)
	}
	listed := agentNamed("finder")
	listed.Tools, listed.HasTools = []string{"lsp"}, true
	if chosen := childNamesOf(t, listed, nil, readOnlyTool("lsp"), execTool); strings.Join(chosen, ",") != "lsp" {
		t.Fatalf("a child whose allow list names lsp got %v", chosen)
	}
}

// A definition file written before read_only still loads, and the model is shown the current
// schema; any other schema is refused.
func TestExecDefinitionFileWithoutReadOnlyStillLoads(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "/s/setting.json"}
	old := tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "mine", InputSchema: execInputSchemaBeforeReadOnly}
	definition, err := requireExecDefinition(config, map[string]tools.Definition{execToolID: old})
	if err != nil {
		t.Fatal(err)
	}
	if matches, _ := sameJSON(definition.InputSchema, execInputSchema); !matches || definition.Description != "mine" {
		t.Fatalf("definition = %s %s", definition.Description, definition.InputSchema)
	}
	if strings.Contains(string(execInputSchemaBeforeReadOnly), execReadOnlyProperty) {
		t.Fatalf("old schema still has read_only: %s", execInputSchemaBeforeReadOnly)
	}
	current := old
	current.InputSchema = execInputSchema
	if _, err := requireExecDefinition(config, map[string]tools.Definition{execToolID: current}); err != nil {
		t.Fatal(err)
	}
	other := old
	other.InputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	if _, err := requireExecDefinition(config, map[string]tools.Definition{execToolID: other}); err == nil {
		t.Fatal("a different exec schema was accepted")
	}
}

// A definition file whose read_only text predates "a session may be set to ignore it" still
// loads, and the model is shown the current text.
func TestExecDefinitionFileWithThePromiseOnlyTextStillLoads(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "/s/setting.json"}
	if matches, _ := sameJSON(execInputSchemaPromiseOnly, execInputSchema); matches {
		t.Fatal("the previous schema equals the current one")
	}
	if !strings.Contains(string(execInputSchemaPromiseOnly), "read_only is your promise not to change files.\"") {
		t.Fatalf("previous schema = %s", execInputSchemaPromiseOnly)
	}
	old := tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "mine", InputSchema: execInputSchemaPromiseOnly}
	definition, err := requireExecDefinition(config, map[string]tools.Definition{execToolID: old})
	if err != nil {
		t.Fatal(err)
	}
	if matches, _ := sameJSON(definition.InputSchema, execInputSchema); !matches || !strings.Contains(string(definition.InputSchema), "plan mode refuses calls with code") {
		t.Fatalf("definition schema = %s", definition.InputSchema)
	}
}

// A definition file with the read_only text of the read_only_calls days still loads, and the
// model is shown the current text.
func TestExecDefinitionFileWithTheIgnorableTextStillLoads(t *testing.T) {
	config := sessionfs.SessionConfig{SettingPath: "/s/setting.json"}
	if matches, _ := sameJSON(execInputSchemaIgnorable, execInputSchema); matches {
		t.Fatal("the previous schema equals the current one")
	}
	old := tools.Definition{ID: execToolID, Source: "native", Name: "exec", Description: "mine", InputSchema: execInputSchemaIgnorable}
	definition, err := requireExecDefinition(config, map[string]tools.Definition{execToolID: old})
	if err != nil {
		t.Fatal(err)
	}
	if matches, _ := sameJSON(definition.InputSchema, execInputSchema); !matches || strings.Contains(string(definition.InputSchema), "may be set to ignore it") {
		t.Fatalf("definition schema = %s", definition.InputSchema)
	}
}
