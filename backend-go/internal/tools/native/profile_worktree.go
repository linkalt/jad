package native

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// DelegatedWorktree is the git worktree a delegated profile run works in: the run that a
// profile_message starts in a profile whose sandbox.isolation.mode is worktree
// (docs/isolation-sandbox-design.md §9.7). It is made and finished like a sub-agent's
// (openWorktree, closeWorktree), and its result carries the same worktree_* lines.
//
// While the run goes the worktree is recorded in the target session's state folder, so a
// server that stops meanwhile finishes it at its next start (RecoverDelegatedWorktrees).
type DelegatedWorktree struct {
	tree *worktree.Tree
	// record is the file the worktree is recorded in; empty without one.
	record string
	// mcp is the run's own MCP servers (StartMCP); stopped before the worktree is finished.
	mcp *runMCP
}

// DelegatedWorktreeResult is what the end of a delegated run's worktree is reported as.
type DelegatedWorktreeResult struct {
	// Information are the worktree_* lines of the profile_message result; Text is them as
	// one line, for an async handle's summary.
	Information []tools.Information
	Text        string
}

// OpenDelegatedWorktree creates the run's worktree from the repository that holds
// execution.WorkspaceRoot and points execution at it, as openWorktree does for a sub-agent.
// settings are the target's tools.native.subagent settings (nil takes the defaults of its
// worktree key); isolation.Base "working" starts the run from the session's uncommitted
// files. The worktree is recorded in stateDir, the target session's state folder, with
// message, the commit message its work gets if a restart has to finish it; an empty stateDir
// keeps no record.
func OpenDelegatedWorktree(ctx context.Context, settings *sessionfs.NativeSubagentSettings, isolation sessionfs.SandboxIsolation, execution *tools.ExecutionContext, stateDir, message string) (*DelegatedWorktree, error) {
	var worktreeSettings sessionfs.NativeSubagentWorktreeSettings
	if settings != nil {
		worktreeSettings = settings.Worktree
	}
	// A collection that failed before is tried again first (RecoverDelegatedWorktrees).
	RecoverDelegatedWorktrees(context.WithoutCancel(ctx), stateDir, settings, nil)
	options := worktreeOptions(worktreeSettings)
	if isolation.Base == sessionfs.SandboxIsolationBaseWorking {
		options.Base = worktree.BaseWorking
	}
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("name the run's worktree: %w", err)
	}
	parentRoot := execution.WorkspaceRoot
	tree, holder, err := openIsolatedTree(ctx, options, isolation.Materialize, isolation.SharePaths, executionSandbox(*execution), parentRoot, "profile-"+hex.EncodeToString(random), "")
	if err != nil {
		return nil, err
	}
	record, err := writeDelegatedWorktreeRecord(stateDir, tree, message)
	if err != nil {
		// Without a record a restart could not finish the worktree, so the run does not start.
		holder.stop()
		if finishErr := tree.Remove(context.WithoutCancel(ctx)); finishErr != nil {
			return nil, fmt.Errorf("%w (and the new worktree at %s could not be removed: %v)", err, tree.Path, finishErr)
		}
		return nil, err
	}
	if execution.WorkspaceOrigin == "" {
		execution.WorkspaceOrigin = parentRoot
	}
	execution.WorkspaceRoot = tree.Workspace
	execution.PathScope = movedScope(execution.PathScope, parentRoot, tree.Workspace)
	registerIsolatedRun(tree)
	if holder != nil {
		attachHolder(tree, holder)
	}
	opened := &DelegatedWorktree{tree: tree, record: record}
	execution.MCPRoute, opened.mcp = openRunMCP(tree, execution.WorkspaceOrigin)
	return opened, nil
}

// Sandbox is sandbox for the run's calls, with its absolute paths moved from origin (the
// session's own folder) to the worktree. A placeholder ({workspace}) is filled in with the
// worktree already; an absolute path is what a target that inherits its caller's sandbox
// carries (D20). So the run writes in its worktree, never in the session's folder:
//
//   - a write path inside origin is moved into the worktree;
//   - a write path that holds origin becomes the worktree (what it allowed beside origin is
//     left out: an isolated run writes only where its work is collected from);
//   - read, hidden and read-only paths inside origin are kept and also moved, so the
//     worktree's copy of a hidden or read-only file stays so.
//
// Nil stays nil.
func (d *DelegatedWorktree) Sandbox(sandbox *sessionfs.SandboxSettings, origin string) *sessionfs.SandboxSettings {
	if sandbox == nil {
		return nil
	}
	origins := []string{filepath.Clean(origin)}
	if canonical := sessionfs.CanonicalRulePath(origin); canonical != origins[0] {
		origins = append(origins, canonical)
	}
	to := d.tree.Workspace
	writes := func(paths []string) []string {
		var moved []string
		for _, path := range paths {
			switch {
			case !filepath.IsAbs(path):
				moved = appendNew(moved, path)
			case withinAny(origins, path):
				moved = appendNew(moved, movePath(origins, to, path))
			case holdsAny(path, origins):
				moved = appendNew(moved, to)
			default:
				moved = appendNew(moved, path)
			}
		}
		return moved
	}
	alsoMoved := func(paths []string) []string {
		var moved []string
		for _, path := range paths {
			moved = appendNew(moved, path)
			if filepath.IsAbs(path) && withinAny(origins, path) {
				moved = appendNew(moved, movePath(origins, to, path))
			}
		}
		return moved
	}
	copied := *sandbox
	copied.WritablePaths = writes(sandbox.WritablePaths)
	copied.PlanWritablePaths = writes(sandbox.PlanWritablePaths)
	if sandbox.ReadPaths != nil {
		copied.ReadPaths = alsoMoved(sandbox.ReadPaths)
	}
	copied.HidePaths = alsoMoved(sandbox.HidePaths)
	copied.ReadOnlyPaths = alsoMoved(sandbox.ReadOnlyPaths)
	copied.SealedPaths = alsoMoved(sandbox.SealedPaths)
	copied.SealedInner = alsoMoved(sandbox.SealedInner)
	// The data folder stays the workspace's own, where the run's writable paths no longer reach.
	copied.ProfileHidden = alsoMoved(sandbox.ProfileHidden)
	copied.ProfileShown = alsoMoved(sandbox.ProfileShown)
	return &copied
}

// holdsAny reports whether one of paths lies inside root.
func holdsAny(root string, paths []string) bool {
	for _, path := range paths {
		if sessionfs.PathWithin(root, path) {
			return true
		}
	}
	return false
}

func appendNew(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// Close commits what the run left in the worktree (in the run's sandbox, from execution:
// the context the run's tools had) and removes the folder; the branch stays when the run
// changed something. Call it once nothing of the run works in the folder any more.
func (d *DelegatedWorktree) Close(ctx context.Context, execution tools.ExecutionContext, message string) DelegatedWorktreeResult {
	defer forgetIsolatedRun(d.tree)
	// The run's MCP servers stop before what the run left is collected.
	d.mcp.end()
	outcome, err := finishIsolatedTree(context.WithoutCancel(ctx), d.tree, message, func() worktree.Launcher { return collectLauncher(execution, d.tree) }, holderExecutable(executionSandbox(execution)), d.tree.Timeout())
	if d.record != "" {
		if outcome.Kept {
			// The collection failed: the folders are kept and so is the record, which a later
			// start or run tries again from (RecoverDelegatedWorktrees); the caller is told the
			// path now.
			if kept, readErr := readDelegatedWorktreeRecord(d.record); readErr == nil {
				_ = keepDelegatedWorktreeRecord(d.record, kept, 1, fmt.Sprint(err))
			}
		} else {
			_ = os.Remove(d.record)
		}
	}
	record := worktreeRecord(outcome)
	if err != nil {
		record.Error = boundedReason(err.Error())
	}
	if outcome.Kept {
		record.Path = d.tree.Path
	}
	view := subagentRunView{worktree: &record}
	return DelegatedWorktreeResult{Information: worktreeInformation(view), Text: worktreeText(view)}
}
