package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// sendWithChoice sends one message to review/reviewer in sandbox choice, from a caller in
// environment (nil for none), and returns the outcome, or the Prepare error.
func sendWithChoice(t *testing.T, f *layoutFixture, choice *sessionfs.SandboxChoice, environment *tools.Environment) (native.ProfileMessageOutcome, error) {
	t.Helper()
	messenger := profileMessenger{resolver: f.service.resolver}
	ctx, cancel := context.WithTimeout(callerContext(t, f, context.Background()), time.Minute)
	defer cancel()
	plan, err := messenger.Prepare(ctx, native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "write it",
		Environment: environment, CallerPlace: sessionfs.SandboxPlace{Workspace: f.cwd, CWD: f.cwd}, Sandbox: choice})
	if err != nil {
		return native.ProfileMessageOutcome{}, err
	}
	run, err := plan.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("the message did not end")
	}
	return run.Outcome(), nil
}

func bwrapChoice(name string, writes ...string) *sessionfs.SandboxChoice {
	return &sessionfs.SandboxChoice{Name: name, Sandbox: &sessionfs.SandboxSettings{Template: name, Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: []string{"exec"},
		ReadPaths: []string{"/"}, WritablePaths: writes, PlanWritablePaths: []string{"{tmp}"}}}
}

func requireBwrap(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bwrap runs on Linux only")
	}
	if out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot run here: %v %s", err, out)
	}
}

// §15.4: a profile_message call with sandbox runs the target in the chosen template: under a
// reader template the target's write fails (read-only file system), and the target's next run
// without a choice is not confined by it.
func TestProfileMessageSandboxChoiceConfinesOnlyThatRun(t *testing.T) {
	requireBwrap(t)
	f := isolatedTargetFixture(t, `{"mode":"none"}`)
	outcome, err := sendWithChoice(t, f, bwrapChoice("reader", "{tmp}"), nil)
	if err != nil || outcome.Err != nil {
		t.Fatalf("prepare %v, outcome %+v", err, outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the target wrote under the reader choice: %v", err)
	}
	transcript, _ := os.ReadFile(boundRunnerOf(t, f, "review/reviewer").config.MessagesPath)
	if !strings.Contains(string(transcript), "Read-only file system") {
		t.Fatalf("the write did not fail as read-only:\n%s", transcript)
	}
	// The same target, messaged without a choice, writes as its own settings allow. Its
	// transcript now holds the first message's exec result, so the script calls exec again
	// while that is the only one.
	write := scriptFunctionCall("exec", map[string]any{"code": "printf 'by the target' > made.txt"})
	outputs := strings.Count(string(transcript), `"function_call_output"`)
	f.script = func(model, body string) string {
		if model == "model-large" && strings.Count(body, `"function_call_output"`) <= outputs {
			return write(model, "")
		}
		return ""
	}
	outcome, err = sendWithChoice(t, f, nil, nil)
	if err != nil || outcome.Err != nil {
		t.Fatalf("plain message: %v %+v", err, outcome)
	}
	if data, err := os.ReadFile(filepath.Join(f.cwd, "made.txt")); err != nil || string(data) != "by the target" {
		t.Fatalf("made.txt after a plain message: %v %q", err, data)
	}
}

// A choice whose template sets isolation.mode worktree runs the target's message in a
// worktree of its own, even though the target's settings do not.
func TestProfileMessageSandboxChoiceIsolatesTheRun(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"none"}`)
	outcome, err := sendWithChoice(t, f, &sessionfs.SandboxChoice{Name: "isolate", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationWorktree}}, nil)
	if err != nil || outcome.Err != nil {
		t.Fatalf("prepare %v, outcome %+v", err, outcome)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the isolated run wrote into the session's folder: %v", err)
	}
	branch := worktreeValue(outcome, "worktree_branch")
	if branch == "" || gitIn(t, f.cwd, "show", branch+":made.txt") != "by the target" {
		t.Fatalf("worktree: %+v", outcome.Worktree)
	}
}

// A choice is refused before the run starts, naming it, when it would widen the target's run:
// one with network for a caller without it, and one that would take an isolated target out
// of its worktree.
func TestProfileMessageSandboxChoiceRefusals(t *testing.T) {
	f := isolatedTargetFixture(t, `{"mode":"worktree"}`)
	online := bwrapChoice("online", "{workspace}", "{tmp}")
	online.Sandbox.Network = true
	caller := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"},
		WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	for name, check := range map[string]struct {
		choice      *sessionfs.SandboxChoice
		environment *tools.Environment
		want        string
	}{
		"network":   {online, &tools.Environment{Sandbox: caller}, "network"},
		"isolation": {&sessionfs.SandboxChoice{Name: "in-place", Isolation: sessionfs.SandboxIsolation{Mode: sessionfs.SandboxIsolationNone}}, nil, "isolation.mode none"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sendWithChoice(t, f, check.choice, check.environment)
			var refused *native.ProfileSandboxError
			if !errors.As(err, &refused) || refused.Choice != check.choice.Name || !strings.Contains(refused.Message, check.want) {
				t.Fatalf("err = %v (%+v)", err, refused)
			}
		})
	}
}

// §15.6: a writer agent called with sandbox reader runs in that template: its write fails
// under bwrap with a read-only file system, the session's folder is unchanged, and the parent
// is told the run's sandbox.
func TestSubagentSandboxChoiceConfinesTheChildUnderBwrap(t *testing.T) {
	requireBwrap(t)
	reader := map[string]any{"schema_version": 1, "description": "Reader: writes nothing but scratch", "enabled": true, "backend": "bwrap", "tools": []string{"exec"},
		"write": map[string]any{"paths": []string{"{tmp}"}, "plan_paths": []string{"{tmp}"}}}
	f := newSubagentFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"skill", "exec"}, "__exec": map[string]any{},
		"__subagent_settings": map[string]any{"sandbox_choice": []string{"reader"}},
		"__sandbox_templates": map[string]map[string]any{"reader": reader},
	})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"write it","sandbox":"reader"}`}
	f.provider.childFirstCall = [2]string{"exec", execWrite("made.txt", "by the child")}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("the child wrote under the reader choice: %v", err)
	}
	child := f.provider.forModel("child-model")
	if last := child[len(child)-1].Text; !strings.Contains(last, "Read-only file system") {
		t.Fatalf("the child's write did not fail as read-only:\n%s", last)
	}
	parent := f.provider.forModel("parent-model")
	if last := parent[len(parent)-1].Text; !strings.Contains(last, "sandbox: reader") {
		t.Fatalf("the parent was not told the run's sandbox:\n%s", last)
	}
}

// confineTarget applies the choice after the caller's narrowing; the chosen sandbox keeps the
// target's own session file read-only beside the template's paths.
func TestConfineTargetWithAChoice(t *testing.T) {
	target := sessionfs.SessionConfig{SettingPath: "/root/sessions/t.json", WorkingDir: "/work/t", StateDir: "/state/t"}
	choice := bwrapChoice("reader", "{tmp}")
	choice.Sandbox.ReadOnlyPaths = []string{"/root/setting.json"}
	got, err := confineTarget(nil, sessionfs.SandboxPlace{}, choice, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sandbox == nil || strings.Join(got.Sandbox.WritablePaths, ",") != "{tmp}" || strings.Join(got.Sandbox.ReadOnlyPaths, ",") != "/root/setting.json,/root/sessions/t.json" {
		t.Fatalf("sandbox = %+v", got.Sandbox)
	}
	if choice.Sandbox.ReadOnlyPaths[0] != "/root/setting.json" || len(choice.Sandbox.ReadOnlyPaths) != 1 {
		t.Fatalf("the choice itself changed: %v", choice.Sandbox.ReadOnlyPaths)
	}
	if got, err := confineTarget(nil, sessionfs.SandboxPlace{}, nil, target, false); err != nil || got.Sandbox != nil {
		t.Fatalf("no caller, no choice: %+v %v", got.Sandbox, err)
	}
}
