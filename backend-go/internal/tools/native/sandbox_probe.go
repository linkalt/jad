package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// ProbeSandbox starts the program true inside sandbox, the way a tool call would start its
// child from workingDir, and reports why it did not run. A bwrap found on the PATH can still
// fail on every call, for example when the host forbids unprivileged user namespaces; a
// file check cannot see that, a trial start can.
func ProbeSandbox(ctx context.Context, sandbox *sessionfs.SandboxSettings, workingDir string) error {
	if sandbox == nil {
		return nil
	}
	program, err := exec.LookPath("true")
	if err != nil {
		return fmt.Errorf("no program true to start in the sandbox: %w", err)
	}
	launch, err := sandboxed(sandbox, sandboxPlace{workspace: workingDir, cwd: workingDir}, "true", program, nil)
	if err != nil {
		return fmt.Errorf("the sandbox program cannot be run: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(sandbox.CheckTimeoutMs)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, launch.resolved, launch.args...)
	command.Args[0] = launch.configured
	command.Dir = launch.dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	// A timeout kills the whole group, so a wrapper's own children cannot keep stderr open.
	configureProcessGroup(command)
	command.Cancel = func() error {
		signalProcessGroup(command, true)
		return nil
	}
	err = command.Run()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("a trial start in the sandbox did not finish within %d ms (setting: sandbox.check_timeout_ms)", sandbox.CheckTimeoutMs)
	}
	reason, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
	if reason == "" {
		reason = err.Error()
	}
	return fmt.Errorf("a trial start in the sandbox failed, so every sandboxed call would fail: %s", reason)
}

// ProbePlanFallback starts the sandbox that plan-mode and read_only exec calls fall back to
// when no sandbox covers exec (sessionfs.PlanFallbackSandbox), as planSandboxWorks does before
// the first such call, and reports why it does not work here.
func ProbePlanFallback(ctx context.Context) error {
	fallback := sessionfs.PlanFallbackSandbox(nil)
	if fallback == nil {
		return errors.New("neither bwrap nor seatbelt runs on this system")
	}
	probed := *fallback.ForPlanMode()
	probed.ReadOnlyPaths, probed.SealedPaths, probed.SealedInner = nil, nil, nil
	probed.ProfileHidden, probed.ProfileShown, probed.ProfileData = nil, nil, ""
	return ProbeSandbox(ctx, &probed, os.TempDir())
}
