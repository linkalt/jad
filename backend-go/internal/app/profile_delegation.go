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
	// lineage is the run lineage the run works under (native.RunLineageOf): the caller's, its
	// session key and the message's own key. Empty for a run the remote API started, whose
	// caller is on another server.
	lineage []string
	// deny is the caller's ExecutionContext.Deny (U3): the run's catalog loses the tools it covers
	// and the run hands it on with the target's own deny. It narrows this run only; the profile's
	// settings and its other runs are unchanged. Nil for a caller on another server.
	deny []string
	// modelKeys is the caller's ExecutionContext.ModelKeyEnvs (T-009): the key variables of the
	// models that may answer the caller and the local runs above it. The run's tools and hooks hide
	// them beside its own models' keys, and every run it starts gets them. Like deny, it holds for
	// this run only; the target's settings and its own runs are unchanged. Nil for a caller on
	// another server.
	modelKeys []string
	// mcpScope is the wrapper key of the run's MCP sandbox when the caller's narrowing changed it
	// (holdMCPScope), with a key of the run's own added for an isolated run; empty when the run's
	// servers start as the profile's own do.
	mcpScope string
}

// targetRunner is the Confine hook of a profile_message run: the target's runner narrowed
// by the caller's sandbox when the caller has one (targetConfiner), and marked as delegated
// so that an isolated target works in a worktree and reports it through report, and narrowed by
// the caller's deny (profileDelegation.deny), and made to hide the caller's model keys
// (profileDelegation.modelKeys). A runner that is not a boundRunner (a test's) builds no tools
// from settings, so it has no environment to narrow by modelKeys.
func targetRunner(confine func(httpapi.RunSession) (httpapi.RunSession, error), caller, message string, report func(native.DelegatedWorktreeResult), lineage, deny, modelKeys []string) func(httpapi.RunSession) (httpapi.RunSession, error) {
	return func(runner httpapi.RunSession) (httpapi.RunSession, error) {
		bound, ok := runner.(boundRunner)
		if !ok && len(deny) > 0 {
			// Its tools cannot be narrowed: refuse rather than run it with what the caller denies.
			return nil, errors.New("the target's run cannot be narrowed by the caller's tools.deny")
		}
		if !ok {
			// Not a session resolved from its settings (a test's runner): nothing to isolate.
			if confine != nil {
				return confine(runner)
			}
			return runner, nil
		}
		ownMCP, _ := mcpSandboxWrapper(bound.config)
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
			lineage:       lineage,
			deny:          deny,
			modelKeys:     modelKeys,
		}
		// A narrowing that changes where the stdio MCP servers start gives the run servers of
		// that narrowing, never the profile's shared ones rewrapped (runMCPManager).
		if runMCP, _ := mcpSandboxWrapper(bound.config); runMCP != ownMCP {
			bound.delegation.mcpScope = runMCP
			if bound.config.Isolation.Mode == sessionfs.SandboxIsolationWorktree {
				// An isolated run calls its worktree's servers and stops the ones it listed its
				// tools from once those started (runLocked): a manager no other run shares.
				bound.delegation.mcpScope += "\x00" + native.NewRunLineageKey("isolated_run")
			}
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

// withLineage puts the run's lineage on ctx, so the handles and sub-agent runs it starts carry
// it: for a run profile_message started, the lineage it was given, and for every run its own key.
func (r boundRunner) withLineage(ctx context.Context, key string) context.Context {
	var lineage []string
	if r.delegation != nil {
		lineage = r.delegation.lineage
	}
	return native.WithRunLineage(ctx, native.ExtendRunLineage(lineage, key))
}
