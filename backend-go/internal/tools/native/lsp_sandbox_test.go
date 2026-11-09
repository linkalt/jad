package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/tools"
)

// With sandbox.tools listing lsp, a language server starts in the sandbox, one per sandbox
// form: a plan-mode call never talks to the writer call's server.
func TestLanguageServerStartsInTheSandbox(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	t.Cleanup(ShutdownLanguageServers)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.fk"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := fakeLSPSettings(t)
	settings.Sandbox = bwrapSandbox()
	tool, err := NewLSPTool(settings)
	if err != nil {
		t.Fatal(err)
	}
	call := func(plan bool) string {
		output, err := tool.Execute(context.Background(), map[string]any{"action": "symbols", "path": "a.fk"}, tools.ExecutionContext{CallID: "lsp-call", WorkspaceRoot: root, SessionKey: "lsp", PlanMode: plan})
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	for _, plan := range []bool{false, true} {
		if output := call(plan); strings.Contains(output, `"status":"error"`) {
			t.Fatalf("plan %v: %s", plan, output)
		}
	}
	languageServers.mu.Lock()
	defer languageServers.mu.Unlock()
	sandboxed := 0
	for _, slot := range languageServers.slots {
		slot.mu.Lock()
		if slot.client != nil && filepath.Base(slot.client.command.Path) == "bwrap" {
			sandboxed++
		}
		slot.mu.Unlock()
	}
	if sandboxed != 2 {
		t.Fatalf("%d language servers run under bwrap, want 2 (the writer's and the plan-mode one)", sandboxed)
	}
}

// An Environment that does not list lsp starts the server outside, whatever the tool's own
// sandbox: the run's environment decides.
func TestCallSandboxFollowsTheRunsEnvironment(t *testing.T) {
	configured := bwrapSandbox()
	own := bwrapSandbox()
	own.Tools = []string{"exec"}
	environment := &tools.Environment{Sandbox: own}
	if got := callSandbox("exec", configured, tools.ExecutionContext{Environment: environment}); got != own {
		t.Fatalf("exec = %+v", got)
	}
	if got := callSandbox("lsp", configured, tools.ExecutionContext{Environment: environment}); got != nil {
		t.Fatalf("lsp not covered = %+v", got)
	}
	if got := callSandbox("lsp", configured, tools.ExecutionContext{}); got != configured {
		t.Fatalf("no environment = %+v", got)
	}
	if got := callSandbox("exec", configured, tools.ExecutionContext{Environment: &tools.Environment{}}); got != nil {
		t.Fatalf("an environment without a sandbox = %+v", got)
	}
}
