package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/session"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/worktree"
)

// profileDelegation marks a bound runner's run as one a profile_message started in the
// target profile (docs/isolation-sandbox-design.md §9.7). Such a run of a profile whose
// sandbox.isolation.mode is worktree works in a git worktree of its own; a user's run of the
// same profile never does (D7).
type profileDelegation struct {
	// isolation is the target profile's own sandbox.isolation, from its settings, or the
	// call's sandbox choice's when that sets isolation.mode worktree.
	isolation sessionfs.SandboxIsolation
	// commitMessage is the message of the commit that holds what the run left.
	commitMessage string
	// report receives the worktree's end once the run is over.
	report func(native.DelegatedWorktreeResult)
}

// targetRunner is the Confine hook of a profile_message run: the target's runner narrowed
// by the caller's sandbox when the caller has one (targetConfiner), and marked as delegated
// so that an isolated target works in a worktree and reports it through report.
func targetRunner(confine func(httpapi.RunSession) (httpapi.RunSession, error), caller, message string, report func(native.DelegatedWorktreeResult)) func(httpapi.RunSession) (httpapi.RunSession, error) {
	return func(runner httpapi.RunSession) (httpapi.RunSession, error) {
		bound, ok := runner.(boundRunner)
		if !ok {
			// Not a session resolved from its settings (a test's runner): nothing to isolate.
			if confine != nil {
				return confine(runner)
			}
			return runner, nil
		}
		if confine != nil {
			confined, err := confine(bound)
			if err != nil {
				return nil, err
			}
			if bound, ok = confined.(boundRunner); !ok {
				return confined, nil
			}
		}
		// Narrowing by the caller's sandbox (D20) changes where the run may write, not
		// whether it is isolated; a sandbox choice whose template sets isolation.mode worktree
		// does (chooseTargetSandbox).
		bound.delegation = &profileDelegation{
			isolation:     bound.config.Isolation,
			commitMessage: fmt.Sprintf("jad profile run %s (message from %s)\n\n%s", bound.config.Profile, caller, message),
			report:        report,
		}
		return bound, nil
	}
}

// delegatedWorktree is the worktree of one delegated run while it goes.
type delegatedWorktree struct {
	runner   boundRunner
	worktree *native.DelegatedWorktree
	options  *session.RunOptions
	// async and since find the async handles the run started, which are stopped before the
	// worktree is finished; async is nil for a session without the async tool.
	async *asyncmgr.Manager
	since time.Time
	once  sync.Once
}

// openDelegatedWorktree creates the run's worktree, points the run's tools at it and drops
// the run's checkpoint.
func (r boundRunner) openDelegatedWorktree(ctx context.Context, options *session.RunOptions) (*delegatedWorktree, error) {
	opened := &delegatedWorktree{runner: r, options: options}
	if r.resolver != nil {
		opened.async = r.resolver.sessionAsyncManager(r.config)
	}
	if opened.async != nil {
		opened.since = opened.async.Now()
	}
	origin := options.ToolExecution.WorkspaceRoot
	tree, err := native.OpenDelegatedWorktree(ctx, r.config.NativeSubagent, r.delegation.isolation, &options.ToolExecution, r.config.StateDir, r.delegation.commitMessage)
	if err != nil {
		return nil, &delegatedWorktreeError{err: err}
	}
	opened.worktree = tree
	if environment := options.ToolExecution.Environment; environment != nil && environment.Sandbox != nil {
		// A sandbox the target inherited from its caller names the session's folder outright;
		// the run writes in its worktree instead.
		moved := *environment
		moved.Sandbox = tree.Sandbox(environment.Sandbox, origin)
		options.ToolExecution.Environment = &moved
	}
	options.Checkpoint = nil
	return opened, nil
}

// close stops what the run left running in the worktree, commits what it left there to its
// branch, removes the folder and reports the end.
func (d *delegatedWorktree) close(ctx context.Context) {
	d.once.Do(func() {
		if d.async != nil {
			d.async.CancelOwnerSince(d.options.ToolExecution.SessionKey, d.since, "profile_run_ended")
		}
		result := d.worktree.Close(ctx, d.options.ToolExecution, d.runner.delegation.commitMessage)
		if d.runner.delegation.report != nil {
			d.runner.delegation.report(result)
		}
	})
}

// delegatedWorktreeError is a delegated run that could not get its worktree. The caller's
// result says so (ClientMessage); git's own message, which names folders, stays in the log.
type delegatedWorktreeError struct{ err error }

func (e *delegatedWorktreeError) Error() string {
	return fmt.Sprintf("create the run's git worktree (sandbox.isolation.mode %s): %v", sessionfs.SandboxIsolationWorktree, e.err)
}

func (e *delegatedWorktreeError) Unwrap() error { return e.err }

func (e *delegatedWorktreeError) ClientMessage() string {
	message := "The profile runs a message in a git worktree of its own (sandbox.isolation.mode " + sessionfs.SandboxIsolationWorktree + "), and the worktree could not be created"
	if errors.Is(e.err, worktree.ErrNotRepository) {
		message += ": the profile's working directory is not in a git repository"
	}
	return message + ". Details are in the server log."
}
