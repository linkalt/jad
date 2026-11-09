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

func execReadOnlyCatalog(t *testing.T, root string) (*tools.Catalog, tools.Tool) {
	t.Helper()
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
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
	output, err := catalog.ExecuteFunctionCall(context.Background(), execROCall(arguments), tools.ExecutionContext{CallID: "c", WorkspaceRoot: root, SessionDir: root})
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

// Plan mode keeps exec without an allow entry: a read_only call runs, any other call that
// runs code is refused before it starts.
func TestPlanModeRunsReadOnlyExecCallsAndRefusesTheOthers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("plan input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, _ := execReadOnlyCatalog(t, root)
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
	output = runExecCall(t, plan, `{"code":"touch made.txt"}`, root)
	if !strings.Contains(output, `"error_code":"plan_mode"`) && !strings.Contains(output, "error_code: plan_mode") {
		t.Fatalf("a call without read_only was not refused:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "made.txt")); err == nil {
		t.Fatal("the refused call ran")
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
		got := callSandbox(sandbox, execution)
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

// With read_only_calls sandboxed, read_only counts only where a bwrap or seatbelt sandbox
// covers exec: with none, a read_only call is a change, so it runs in order and plan mode
// refuses it.
func TestExecReadOnlyCallsSandboxedNeedsAnEnforcingSandbox(t *testing.T) {
	call := execROCall(`{"code":"ls","read_only":true}`)
	for name, tc := range map[string]struct {
		sandbox *sessionfs.SandboxSettings
		change  bool
	}{
		"no sandbox":      {nil, true},
		"command backend": {&sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendCommand, Command: []string{"env"}}, true},
		"bwrap":           {bwrapSandbox(), false},
	} {
		root := t.TempDir()
		settings := execTestSettings()
		settings.Child = bashTestSettings(t, root)
		settings.Child.Sandbox = tc.sandbox
		settings.ReadOnlyCalls = sessionfs.NativeExecReadOnlyCallsSandboxed
		tool, err := NewExecTool(settings, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := tools.NewCatalog([]tools.Tool{tool})
		if err != nil {
			t.Fatal(err)
		}
		if got := catalog.ChangesState(call); got != tc.change {
			t.Errorf("%s: ChangesState = %v, want %v", name, got, tc.change)
		}
		if tc.sandbox == nil {
			plan, err := catalog.PlanMode(nil)
			if err != nil {
				t.Fatal(err)
			}
			if output := runExecCall(t, plan, `{"code":"echo ran","read_only":true}`, root); !strings.Contains(output, "plan_mode") {
				t.Errorf("%s: plan mode ran a read_only call it cannot enforce:\n%s", name, output)
			}
		}
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
	if matches, _ := sameJSON(definition.InputSchema, execInputSchema); !matches || !strings.Contains(string(definition.InputSchema), "may be set to ignore it") {
		t.Fatalf("definition schema = %s", definition.InputSchema)
	}
}
