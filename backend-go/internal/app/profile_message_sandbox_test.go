package app

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

// D20: a profile_message target runs inside its caller's sandbox.
func TestProfileMessageTargetIsConfinedByTheCaller(t *testing.T) {
	caller := &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}, HidePaths: []string{"{home}/.ssh"}}
	callerPlace := sessionfs.SandboxPlace{Workspace: "/work/a", CWD: "/work/a", SessionDir: "/state/a"}
	exec := &sessionfs.NativeExecSettings{}
	target := sessionfs.SessionConfig{WorkingDir: "/work/a/sub", StateDir: "/state/b", SettingPath: "/sessions/b.json", NativeExec: exec}

	// No caller sandbox: the target as it is.
	if got, err := confineTarget(nil, callerPlace, nil, target); err != nil || got.Sandbox != nil {
		t.Fatalf("no caller sandbox: %+v %v", got.Sandbox, err)
	}
	// A target without a sandbox takes the caller's, with the caller's paths.
	got, err := confineTarget(caller, callerPlace, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sandbox.WritablePaths, []string{"/work/a", "{tmp}"}) || got.NativeExec.Child.Sandbox != got.Sandbox || exec.Child.Sandbox != nil {
		t.Fatalf("inherited %+v, exec %+v, original exec %+v", got.Sandbox, got.NativeExec.Child.Sandbox, exec.Child.Sandbox)
	}
	if !slices.Contains(got.Sandbox.ReadOnlyPaths, "/sessions/b.json") || slices.Contains(caller.ReadOnlyPaths, "/sessions/b.json") {
		t.Fatalf("the target's session file is not kept read-only: %v", got.Sandbox.ReadOnlyPaths)
	}
	if env := runEnvironment(got); env == nil || env.Sandbox != got.Sandbox {
		t.Fatal("the run's environment is not the confined sandbox")
	}
	// A target whose own sandbox stays inside the caller's runs in its own, with the caller's
	// hidden paths too.
	own := &sessionfs.SandboxSettings{Backend: "bwrap", Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{workspace}", "{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	target.Sandbox = own
	if got, err = confineTarget(caller, callerPlace, nil, target); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sandbox.WritablePaths, own.WritablePaths) || len(got.Sandbox.HidePaths) != 1 {
		t.Fatalf("narrowed = %+v", got.Sandbox)
	}
	// A target in another folder writes outside the caller's workspace: refused.
	target.WorkingDir = "/work/b"
	if _, err := confineTarget(caller, callerPlace, nil, target); err == nil || !strings.HasPrefix(err.Error(), sessionfs.SandboxWidensPrefix) {
		t.Fatalf("a wider target: %v", err)
	}
	// So is a target with network when the caller has none.
	target.WorkingDir, own.Network = "/work/a", true
	if _, err := confineTarget(caller, callerPlace, nil, target); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("a target with network: %v", err)
	}
}

// Prepare gives the plan of a sandboxed caller the hook that confines the target's run, and
// the hook narrows the runner the run starts with.
func TestProfileMessagePlanConfinesTheTargetRun(t *testing.T) {
	f := newLayoutFixture(t)
	messenger := profileMessenger{resolver: f.service.resolver}
	caller := &sessionfs.SandboxSettings{Backend: "command", Command: []string{"env"}, Tools: []string{"exec"}, ReadPaths: []string{"/"}, WritablePaths: []string{"{tmp}"}, PlanWritablePaths: []string{"{tmp}"}}
	request := native.ProfileMessageRequest{OwnerKey: profileMessageOwner, Target: "review/reviewer", Message: "hi",
		Environment: &tools.Environment{Sandbox: caller}, CallerPlace: sessionfs.SandboxPlace{Workspace: "/work/a", CWD: "/work/a"}}
	plan, err := messenger.Prepare(callerContext(t, f, context.Background()), request)
	if err != nil {
		t.Fatal(err)
	}
	confine := plan.(*profileMessagePlan).confine
	if confine == nil || plan.(*profileMessagePlan).request(&profileMessageRun{}, false).Confine == nil {
		t.Fatal("a sandboxed caller's plan, or the run request it makes, has no confine hook")
	}
	resolved, err := f.service.resolver.Resolve(context.Background(), plan.(*profileMessagePlan).reference, "review/reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := confine(resolved.Runner)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox := runner.(boundRunner).config.Sandbox; sandbox == nil || !reflect.DeepEqual(sandbox.WritablePaths, []string{"{tmp}"}) {
		t.Fatalf("the target run's sandbox = %+v", sandbox)
	}
	// Without a caller sandbox nothing is confined.
	request.Environment = nil
	if plan, err = messenger.Prepare(callerContext(t, f, context.Background()), request); err != nil || plan.(*profileMessagePlan).confine != nil {
		t.Fatalf("an unsandboxed caller: %v", err)
	}
}
