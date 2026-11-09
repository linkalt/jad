package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// The in-process readers mirror the run's environment: a hidden path, a path outside the read
// paths and another run's worktree are refused as the sandbox would not show them.
func TestInProcessReadersMirrorTheEnvironment(t *testing.T) {
	useOwnTempDir(t)
	root := gitRepository(t)
	outside := t.TempDir()
	for _, file := range []string{filepath.Join(root, ".env"), filepath.Join(outside, "o.txt")} {
		if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	options := worktree.Options{Git: "git", BranchPrefix: "jad/", AuthorName: "t", AuthorEmail: "t@t", Timeout: 30_000_000_000}
	other, err := worktree.Create(context.Background(), options, root, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{{Action: "allow", Path: "/"}}}
	sandbox := bwrapSandbox()
	sandbox.HidePaths = []string{"{workspace}/.env"}
	readable := func(execution tools.ExecutionContext, path string) bool {
		_, failure := resolveExistingTarget(path, root, scopedPolicy(policy, execution), "c")
		return failure == ""
	}
	none := tools.ExecutionContext{WorkspaceRoot: root}
	for _, path := range []string{".env", filepath.Join(outside, "o.txt"), filepath.Join(other.Workspace, "a.txt")} {
		if !readable(none, path) {
			t.Fatalf("without an environment %s was refused", path)
		}
	}
	whole := tools.ExecutionContext{WorkspaceRoot: root, Environment: &tools.Environment{Sandbox: sandbox}}
	if readable(whole, ".env") || readable(whole, filepath.Join(other.Workspace, "a.txt")) {
		t.Fatal("a hidden path or another run's worktree was readable")
	}
	if !readable(whole, "a.txt") || !readable(whole, filepath.Join(outside, "o.txt")) {
		t.Fatal("the whole-file-system environment refused a visible path")
	}
	restricted := *sandbox
	restricted.ReadPaths = []string{"{workspace}"}
	narrow := tools.ExecutionContext{WorkspaceRoot: root, Environment: &tools.Environment{Sandbox: &restricted}}
	if readable(narrow, filepath.Join(outside, "o.txt")) || !readable(narrow, "a.txt") {
		t.Fatal("read paths were not mirrored")
	}
	// The run in the other worktree sees its own checkout.
	own := tools.ExecutionContext{WorkspaceRoot: other.Workspace, Environment: &tools.Environment{Sandbox: sandbox}}
	if _, failure := resolveExistingTarget("a.txt", other.Workspace, scopedPolicy(policy, own), "c"); failure != "" {
		t.Fatalf("a run's own worktree was refused: %s", failure)
	}
}

// A sub-agent with a sandbox of its own runs in it, narrowed by the calling run's; one that
// would reach more than its caller does not start.
func TestChildEnvironmentIsNarrowedByTheCaller(t *testing.T) {
	parent := bwrapSandbox()
	parent.WritablePaths, parent.PlanWritablePaths = []string{"{workspace}", "{tmp}"}, []string{"{tmp}"}
	reader := bwrapSandbox()
	reader.WritablePaths, reader.PlanWritablePaths = []string{"{tmp}"}, []string{"{tmp}"}
	execution := tools.ExecutionContext{Environment: &tools.Environment{Sandbox: parent}}
	got, err := childEnvironment(execution, sessionfs.NativeSubagentAgent{Sandbox: reader})
	if err != nil || got.Sandbox.WritablePaths[0] != "{tmp}" {
		t.Fatalf("reader = %+v %v", got, err)
	}
	if got, _ := childEnvironment(execution, sessionfs.NativeSubagentAgent{}); got != execution.Environment {
		t.Fatal("an agent without a sandbox does not keep its caller's environment")
	}
	// The reader starts a writer: refused, the writer reaches more than the reader.
	writer := bwrapSandbox()
	_, err = childEnvironment(tools.ExecutionContext{Environment: got}, sessionfs.NativeSubagentAgent{Sandbox: writer})
	if err == nil || !strings.HasPrefix(err.Error(), sessionfs.SandboxWidensPrefix) {
		t.Fatalf("a wider nested agent: %v", err)
	}
}

// A sandboxed agent that writes only scratch, and cannot message a profile, is scheduled as
// a reader; any wider write path, or profile_message, makes it a writer again.
func TestSandboxReaderAgent(t *testing.T) {
	reader := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Tools: []string{"exec", "tool_smith"}, WritablePaths: []string{"{tmp}"}, PlanWritablePaths: []string{"{tmp}/x"}}
	agent := sessionfs.NativeSubagentAgent{Sandbox: reader, NoProfileMessage: true}
	if !agent.SandboxReader() || agent.ChangesWorkspace() {
		t.Fatal("a scratch-only sandboxed agent is not a reader")
	}
	for name, change := range map[string]func(*sessionfs.NativeSubagentAgent){
		"profile_message": func(a *sessionfs.NativeSubagentAgent) { a.NoProfileMessage = false },
		"workspace": func(a *sessionfs.NativeSubagentAgent) {
			s := *reader
			s.WritablePaths = []string{"{workspace}"}
			a.Sandbox = &s
		},
		"command backend": func(a *sessionfs.NativeSubagentAgent) {
			s := *reader
			s.Backend = sessionfs.SandboxBackendCommand
			a.Sandbox = &s
		},
		"no sandbox": func(a *sessionfs.NativeSubagentAgent) { a.Sandbox = nil },
		// exec outside the sandbox writes wherever it likes.
		"exec not covered": func(a *sessionfs.NativeSubagentAgent) {
			s := *reader
			s.Tools = []string{"tool_smith"}
			a.Sandbox = &s
		},
		"listed exec not covered": func(a *sessionfs.NativeSubagentAgent) {
			s := *reader
			s.Tools = []string{"tool_smith"}
			a.Sandbox = &s
			a.Tools, a.HasTools = []string{"read", "exec"}, true
		},
	} {
		changed := agent
		change(&changed)
		if changed.SandboxReader() || !changed.ChangesWorkspace() {
			t.Errorf("%s: still a reader", name)
		}
	} // An agent that lists exec but not tool_smith needs only exec covered.
	listed := agent
	listed.Tools, listed.HasTools = []string{"read", "exec"}, true
	onlyExec := *reader
	onlyExec.Tools = []string{"exec"}
	listed.Sandbox = &onlyExec
	if !listed.SandboxReader() {
		t.Error("an exec-only agent in a sandbox covering exec is not a reader")
	}
}

// A plan-mode caller narrows a profile_message target by the plan-mode form of its sandbox.
func TestProfileMessageCallerEnvironmentFollowsPlanMode(t *testing.T) {
	sandbox := bwrapSandbox()
	sandbox.PlanWritablePaths = []string{"{tmp}"}
	environment := &tools.Environment{Sandbox: sandbox}
	if got := callerEnvironment(tools.ExecutionContext{Environment: environment}); got != environment {
		t.Fatal("a writer call's environment changed")
	}
	got := callerEnvironment(tools.ExecutionContext{Environment: environment, PlanMode: true})
	if got == environment || len(got.Sandbox.WritablePaths) != 1 || got.Sandbox.WritablePaths[0] != "{tmp}" {
		t.Fatalf("plan-mode caller environment = %+v", got.Sandbox)
	}
	if callerEnvironment(tools.ExecutionContext{PlanMode: true}) != nil {
		t.Fatal("no environment became one")
	}
}
