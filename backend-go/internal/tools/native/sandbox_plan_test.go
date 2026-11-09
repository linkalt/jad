package native

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func planBashCall(t *testing.T, catalog *tools.Catalog, callID, command, root string) string {
	t.Helper()
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": callID, "name": "exec", "arguments": `{"lang":"bash","code":` + strconvQuote(command) + `}`})
	output, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{WorkspaceRoot: root, SessionDir: root})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// An exec call an allow entry keeps in plan mode runs with the project read-only: the same call
// outside plan mode writes it. The temp folder stays writable.
func TestPlanModeRunsSandboxedCallsWithTheProjectReadOnly(t *testing.T) {
	requireBwrap(t)
	root, _, _ := sandboxDirs(t)
	settings := execTestSettings()
	settings.Child = bashTestSettings(t, root)
	settings.Child.Sandbox = bwrapSandbox()
	settings.Child.Sandbox.PlanWritablePaths = []string{"{tmp}"}
	execTool, err := NewExecTool(settings, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{execTool})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanMode([]string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	command := `touch plan.txt 2>&1; touch "$TMPDIR/plan-tmp.txt" && echo wrote-tmp`
	output := planBashCall(t, plan, "plan-call", command, root)
	if !strings.Contains(output, "Read-only file system") || !strings.Contains(output, "wrote-tmp") {
		t.Fatalf("plan-mode call output:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "plan.txt")); err == nil {
		t.Fatal("a plan-mode call wrote the project")
	}
	output = planBashCall(t, catalog, "work-call", `touch work.txt && echo wrote-project`, root)
	if !strings.Contains(output, "wrote-project") {
		t.Fatalf("work call output:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(root, "work.txt")); err != nil {
		t.Fatalf("the call outside plan mode did not write the project: %v", err)
	}
}

func TestCallSandboxUsesThePlanPathsOnlyInPlanMode(t *testing.T) {
	sandbox := bwrapSandbox()
	sandbox.PlanWritablePaths = []string{"{tmp}"}
	if got := callSandbox(sandbox, tools.ExecutionContext{}); got != sandbox {
		t.Fatalf("outside plan mode the sandbox changed: %+v", got)
	}
	plan := callSandbox(sandbox, tools.ExecutionContext{PlanMode: true})
	if !reflect.DeepEqual(plan.WritablePaths, []string{"{tmp}"}) || !reflect.DeepEqual(sandbox.WritablePaths, []string{"{workspace}", "{git_common_dir}", "{tmp}"}) {
		t.Fatalf("plan writable = %v, configured = %v", plan.WritablePaths, sandbox.WritablePaths)
	}
	// A plan-mode exec standby is a different process from a work one.
	place := sandboxPlace{workspace: "/w"}
	if sandboxKey(plan, place) == sandboxKey(sandbox, place) {
		t.Fatal("the plan sandbox shares a standby key with the work sandbox")
	}
	if callSandbox(nil, tools.ExecutionContext{PlanMode: true}) != nil {
		t.Fatal("no sandbox became one")
	}
	command := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendCommand, Command: []string{"firejail"}}
	if callSandbox(command, tools.ExecutionContext{PlanMode: true}) != command {
		t.Fatal("a command backend was changed")
	}
	if got := sandboxDescription(sandbox); !strings.Contains(got, "In plan mode, writable: $TMPDIR (this session's own temp folder).") {
		t.Fatalf("description = %s", got)
	}
	sandbox.PlanWritablePaths = sandbox.WritablePaths
	if got := sandboxDescription(sandbox); strings.Contains(got, "In plan mode") {
		t.Fatalf("description names plan paths equal to the others: %s", got)
	}
}
