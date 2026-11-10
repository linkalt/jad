package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// subagentWorktree is what a run with isolation worktree records about its worktree. It
// is written into the manifest, so the branch is still known after a restart.
type subagentWorktree struct {
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
	// Start is the snapshot of the session's working state a run of base "working" started
	// from (empty when it started from Base).
	Start string `json:"start,omitempty"`
	// Workspace, Admin, Common and Top are the folders jad made the worktree with, so a run
	// the server lost is finished from them, not from what the run left in the worktree.
	Workspace string `json:"workspace,omitempty"`
	Admin     string `json:"admin,omitempty"`
	Common    string `json:"common,omitempty"`
	Top       string `json:"top,omitempty"`
	// Commit is the branch's last commit once the run ended; Changed reports whether it
	// differs from Base. A run that changed nothing has no branch left.
	Commit  string `json:"commit,omitempty"`
	Changed bool   `json:"changed,omitempty"`
	Stat    string `json:"stat,omitempty"`
	// Path is the worktree folder while the run goes, and after its end only when the
	// folder could not be cleaned up (Error says why).
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
	// Attempts is how many times collecting the run's work has failed (the folders are kept
	// and tried again at the next server start and before the next isolated run, until
	// worktree.collect_attempts is reached; then they stay for manual recovery).
	Attempts int `json:"attempts,omitempty"`
	// Note says why the worktree started where it did, when that is not what was asked.
	Note string `json:"note,omitempty"`
	// Target is the session's HEAD the branch was checked against when the run ended, and
	// Conflicts the paths a merge into it would conflict on; CheckError says why the check
	// did not run. OnWorkingState says the branch also holds the working-state snapshot.
	Target         string   `json:"target,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
	CheckError     string   `json:"check_error,omitempty"`
	OnWorkingState bool     `json:"on_working_state,omitempty"`
	// Uncommitted is the check against the session's uncommitted files when the run ended
	// (W′); nil when the session had none.
	Uncommitted *worktreeUncommitted `json:"uncommitted,omitempty"`
}

// worktreeUncommitted is worktree.UncommittedCheck as a run records it.
type worktreeUncommitted struct {
	Working   string   `json:"working,omitempty"`
	Overlap   []string `json:"overlap,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// worktreeRecord is what a run records of its worktree's end.
func worktreeRecord(outcome worktree.Outcome) subagentWorktree {
	record := subagentWorktree{Branch: outcome.Branch, Base: outcome.Base, Start: outcome.Start, Commit: outcome.Commit, Changed: outcome.Changed, Stat: outcome.Stat,
		Target: outcome.Target, Conflicts: outcome.Conflicts, CheckError: outcome.CheckError, OnWorkingState: outcome.OnWorkingState}
	if check := outcome.Uncommitted; check != nil {
		record.Uncommitted = &worktreeUncommitted{Working: check.Working, Overlap: check.Overlap, Conflicts: check.Conflicts, Error: check.Error}
	}
	return record
}

// state is what worktree.Open needs to finish the record's worktree.
func (w subagentWorktree) state() worktree.State {
	return worktree.State{Path: w.Path, Workspace: w.Workspace, Branch: w.Branch, Base: w.Base, Start: w.Start, Admin: w.Admin, Common: w.Common, Top: w.Top}
}

// subagentWorktreeStart asks the manager to run the child in a worktree of its own.
type subagentWorktreeStart struct {
	options worktree.Options
	// materialize and share are the agent's sandbox.isolation.materialize and share_paths.
	materialize string
	share       []string
	// from is the revision the worktree starts at; empty is HEAD. A resume passes the
	// branch of the run it continues.
	from string
}

// worktreeOptions turns the settings into the options of the git commands. A field left
// empty in settings built in code takes the loader's default.
func worktreeOptions(settings sessionfs.NativeSubagentWorktreeSettings) worktree.Options {
	defaults := sessionfs.DefaultNativeSubagentWorktreeSettings()
	pick := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	timeout := settings.TimeoutMS
	if timeout <= 0 {
		timeout = defaults.TimeoutMS
	}
	return worktree.Options{
		Git:          pick(settings.Git, defaults.Git),
		Dir:          settings.Dir,
		BranchPrefix: pick(settings.BranchPrefix, defaults.BranchPrefix),
		AuthorName:   pick(settings.AuthorName, defaults.AuthorName),
		AuthorEmail:  pick(settings.AuthorEmail, defaults.AuthorEmail),
		CommitHooks:  settings.CommitHooks,
		Timeout:      time.Duration(timeout) * time.Millisecond,

		CollectAttempts: collectAttempts(settings.CollectAttempts),
	}
}

// collectAttempts is the configured number of collection attempts, or the default when a
// settings value built in code leaves it unset.
func collectAttempts(configured int) int {
	if configured > 0 {
		return configured
	}
	return sessionfs.DefaultNativeSubagentWorktreeSettings().CollectAttempts
}

// openWorktree creates the run's worktree and points the child's tools at it: the
// workspace root becomes the session's folder inside the worktree, the session's own
// folder becomes the origin its path rules are moved from, and the agent's path scope is
// moved the same way.
func (m *SubagentManager) openWorktree(ctx context.Context, run *subagentRun, spec *subagentStart) (*worktree.Tree, error) {
	// A collection that failed before is tried again first (recoverWorktree).
	m.retryPendingWorktrees(ctx, spec.worktree.options)
	execution := &spec.request.Execution
	parentRoot := execution.WorkspaceRoot
	record := subagentWorktree{}
	from := spec.worktree.from
	if from != "" && !worktree.BranchExists(ctx, spec.worktree.options, parentRoot, from) {
		record.Note = fmt.Sprintf("the branch %s of the resumed run is gone, so this run starts from HEAD", from)
		from = ""
	}
	tree, holder, err := openIsolatedTree(ctx, spec.worktree.options, spec.worktree.materialize, spec.worktree.share, executionSandbox(*execution), parentRoot, run.id, from)
	if err != nil {
		return nil, err
	}
	if execution.WorkspaceOrigin == "" {
		execution.WorkspaceOrigin = parentRoot
	}
	execution.WorkspaceRoot = tree.Workspace
	execution.PathScope = movedScope(execution.PathScope, parentRoot, tree.Workspace)
	state := tree.State()
	record.Branch, record.Base, record.Path = tree.Branch, tree.Base, tree.Path
	record.Workspace, record.Admin, record.Common, record.Top = state.Workspace, state.Admin, state.Common, state.Top
	if tree.Start != tree.Base {
		record.Start = tree.Start
	}
	registerIsolatedRun(tree)
	if holder != nil {
		attachHolder(tree, holder)
	}
	// The run's MCP servers start in the worktree when the run needs them (startRunMCP).
	execution.MCPRoute, _ = openRunMCP(tree, execution.WorkspaceOrigin)
	run.setWorktree(record)
	return tree, nil
}

// closeWorktree commits what the child left in its worktree and removes the folder; the
// branch stays when the run changed something. It runs after the child and its processes
// have stopped, and also when the run was stopped or the server is shutting down, so the
// work is never left behind in a folder nobody knows of. Each git command is bounded by
// the worktree timeout. The commands that read what the run left start inside the run's
// sandbox (collectLauncher, with execution the child's context).
func (m *SubagentManager) closeWorktree(run *subagentRun, tree *worktree.Tree, execution tools.ExecutionContext) {
	defer forgetIsolatedRun(tree)
	// The run's own MCP servers stop first: nothing of the run may write while it is collected.
	endRunMCP(tree)
	view := run.view()
	message := fmt.Sprintf("jad sub-agent %s (run %s)\n\n%s", view.agent, view.id, view.task)
	finishWorktree(context.WithoutCancel(run.ctx), run, tree, func(ctx context.Context) (worktree.Outcome, error) {
		return finishIsolatedTree(ctx, tree, message, func() worktree.Launcher { return collectLauncher(execution, tree) }, holderExecutable(executionSandbox(execution)), tree.Timeout())
	})
}

// recoverWorktree finishes the worktree of a run that still has one: one the last server lost
// while it went, or one whose collection failed (the folders were kept, Attempts counts the
// failures). It does what closeWorktree would have: what the run left there is committed to its
// branch and the folder is removed. A folder that is gone, or a finish that fails, is recorded on
// the run (the folder's path stays in the record, with one more attempt, while the folder is
// still there). A run that has failed options.CollectAttempts times is left as it is, for manual
// recovery, and logged; nothing is deleted. No sandbox of the run is known at startup: the
// commands run on the host with the folders named outright (worktree.Tree.Finish without a
// launcher), or, for an overlay run, in a holder of their own (recoverIsolatedTree).
func recoverWorktree(ctx context.Context, run *subagentRun, options worktree.Options) {
	view := run.view()
	record := view.worktree
	if record == nil || record.Path == "" {
		return
	}
	if record.Attempts >= options.CollectAttempts && options.CollectAttempts > 0 {
		slog.Default().Warn("a sub-agent run's worktree could not be collected and is kept for manual recovery", "run", view.id, "branch", record.Branch, "path", record.Path, "attempts", record.Attempts, "error", record.Error)
		return
	}
	tree, err := worktree.Open(ctx, options, record.state())
	if err != nil {
		if errors.Is(err, worktree.ErrHeld) {
			// A running process still holds the layers (another server): not this one's to finish.
			return
		}
		run.mu.Lock()
		failed := *run.worktree
		failed.Error = boundedReason("the server stopped while the run was going, and its worktree could not be finished: " + err.Error())
		if errors.Is(err, worktree.ErrGone) {
			failed.Path = ""
		} else {
			failed.Attempts++
			slog.Default().Warn("a sub-agent run's worktree could not be opened to collect it; it is kept", "run", view.id, "path", record.Path, "attempts", failed.Attempts, "error", err)
		}
		run.worktree = &failed
		run.mu.Unlock()
		run.store.save(run)
		return
	}
	message := fmt.Sprintf("jad sub-agent %s (run %s, finished after the server stopped)\n\n%s", view.agent, view.id, view.task)
	finishWorktree(ctx, run, tree, func(ctx context.Context) (worktree.Outcome, error) {
		return recoverIsolatedTree(ctx, tree, message, options.Timeout)
	})
	if after := run.view().worktree; after != nil && after.Path != "" && after.Attempts >= options.CollectAttempts {
		slog.Default().Warn("a sub-agent run's worktree could not be collected; it is kept for manual recovery and not tried again", "run", view.id, "branch", after.Branch, "path", after.Path, "attempts", after.Attempts, "error", after.Error)
	}
}

// retryPendingWorktrees tries again, before a new isolated run starts, the collection of the
// worktrees of finished runs whose collection failed (recoverWorktree); a run being retried is
// not retried twice at once.
func (m *SubagentManager) retryPendingWorktrees(ctx context.Context, options worktree.Options) {
	m.mu.Lock()
	runs := make([]*subagentRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	m.mu.Unlock()
	for _, run := range runs {
		if !run.beginCollectionRetry(options.CollectAttempts) {
			continue
		}
		recoverWorktree(context.WithoutCancel(ctx), run, options)
		run.mu.Lock()
		run.retrying = false
		run.mu.Unlock()
	}
}

// beginCollectionRetry reports whether the run is over, still has a worktree whose collection
// failed fewer than limit times and is not being retried, and marks it as being retried.
func (r *subagentRun) beginCollectionRetry(limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retrying || r.worktree == nil || r.worktree.Path == "" || r.worktree.Attempts == 0 || (limit > 0 && r.worktree.Attempts >= limit) {
		return false
	}
	if r.state == subagentRunQueued || r.state == subagentRunRunning {
		return false
	}
	r.retrying = true
	return true
}

// finishWorktree commits and removes tree with finish and records the outcome on run.
func finishWorktree(ctx context.Context, run *subagentRun, tree *worktree.Tree, finish func(context.Context) (worktree.Outcome, error)) {
	outcome, err := finish(ctx)
	run.mu.Lock()
	record := worktreeRecord(outcome)
	if run.worktree != nil {
		record.Note = run.worktree.Note
	}
	if err != nil {
		record.Error = boundedReason(err.Error())
		record.Attempts = 1
		if run.worktree != nil {
			record.Attempts = run.worktree.Attempts + 1
		}
	}
	if outcome.Kept {
		// Still there: a later start can finish it from the same folders.
		state := tree.State()
		record.Path, record.Workspace, record.Admin, record.Common, record.Top = state.Path, state.Workspace, state.Admin, state.Common, state.Top
	}
	run.worktree = &record
	run.mu.Unlock()
	run.store.save(run)
}

func (r *subagentRun) setWorktree(record subagentWorktree) {
	r.mu.Lock()
	r.worktree = &record
	r.mu.Unlock()
	r.store.save(r)
}

// movedScope is a path scope whose entries inside from are moved to the same place
// under to. The scope was made absolute from the caller's workspace root.
func movedScope(scope *tools.PathScope, from, to string) *tools.PathScope {
	if scope == nil {
		return nil
	}
	origins := []string{filepath.Clean(from)}
	if canonical := sessionfs.CanonicalRulePath(from); canonical != origins[0] {
		origins = append(origins, canonical)
	}
	moved := &tools.PathScope{}
	for _, entry := range scope.Allow {
		moved.Allow = append(moved.Allow, movePath(origins, to, scopeRoot(from, entry)))
	}
	for _, entry := range scope.Deny {
		moved.Deny = append(moved.Deny, movePath(origins, to, scopeRoot(from, entry)))
	}
	return moved
}

// worktreeInformation is what a run's result says about its worktree: the branch that
// holds its changes and how to use it, or that it changed nothing.
func worktreeInformation(run subagentRunView) []tools.Information {
	record := run.worktree
	if record == nil {
		return nil
	}
	var information []tools.Information
	switch {
	case record.Commit == "" && record.Error == "":
		// The run is still going: its worktree is where it works.
		information = append(information, tools.Information{Key: "worktree_branch", Value: record.Branch})
	case record.Changed:
		information = append(information,
			tools.Information{Key: "worktree_branch", Value: record.Branch},
			tools.Information{Key: "worktree_base", Value: record.Base},
			tools.Information{Key: "worktree_changes", Value: record.Stat},
			tools.Information{Key: "worktree_next", Value: fmt.Sprintf("the session's own files are unchanged; review with `git diff %s %s`, take the work with `git merge %s` (or cherry-pick), or drop it with `git branch -D %s`", shortCommit(record.diffBase()), record.Branch, record.Branch, record.Branch)})
		if record.Target != "" {
			information = append(information, tools.Information{Key: "worktree_target", Value: record.Target})
		}
		information = append(information, tools.Information{Key: "worktree_conflicts", Value: record.conflictsText()})
		if record.Uncommitted != nil {
			information = append(information, tools.Information{Key: "worktree_uncommitted", Value: record.uncommittedText()})
		}
		if record.Start != "" {
			information = append(information, tools.Information{Key: "worktree_start", Value: record.startText()})
		}
	case record.Error == "":
		information = append(information, tools.Information{Key: "worktree_changes", Value: "none (its worktree and branch were removed)"})
	}
	if record.Error != "" {
		information = append(information, tools.Information{Key: "worktree_error", Value: record.Error})
		if record.Path != "" {
			information = append(information, tools.Information{Key: "worktree_path", Value: record.Path})
		}
		if record.Branch != "" {
			information = append(information, tools.Information{Key: "worktree_branch", Value: record.Branch})
		}
	}
	if record.Note != "" {
		information = append(information, tools.Information{Key: "worktree_note", Value: record.Note})
	}
	return information
}

// worktreeText is worktreeInformation as one line, for the async handle's summary and
// status; empty for a run without a worktree.
func worktreeText(run subagentRunView) string {
	record := run.worktree
	if record == nil {
		return ""
	}
	var text string
	switch {
	case record.Commit == "" && record.Error == "":
		text = "works in worktree branch " + record.Branch
	case record.Changed:
		text = fmt.Sprintf("worktree branch %s (from %s): %s. The session's files are unchanged: review with `git diff %s %s`, take the work with `git merge %s`, drop it with `git branch -D %s`. Conflicts with HEAD: %s",
			record.Branch, shortCommit(record.Base), record.Stat, shortCommit(record.diffBase()), record.Branch, record.Branch, record.Branch, record.conflictsText())
		if record.Uncommitted != nil {
			text += ". Uncommitted files of the session: " + record.uncommittedText()
		}
		if record.Start != "" {
			text += ". " + record.startText()
		}
	case record.Error == "":
		text = "worktree: no changes (its worktree and branch were removed)"
	}
	if record.Error != "" {
		text = "worktree error: " + record.Error
		if record.Path != "" {
			text += "; the worktree is kept at " + record.Path
		}
	}
	if record.Note != "" {
		text += " (" + record.Note + ")"
	}
	return text
}

// diffBase is the commit the branch's changes are counted from: Base, or the working-state
// snapshot when the branch could not be moved off it.
func (w subagentWorktree) diffBase() string {
	if w.OnWorkingState && w.Start != "" {
		return w.Start
	}
	return w.Base
}

// conflictsText says what merging the branch into the session's HEAD (Target) would
// conflict on, as the run's end found it.
func (w subagentWorktree) conflictsText() string {
	switch {
	case w.CheckError != "":
		return "not checked (" + w.CheckError + ")"
	case len(w.Conflicts) == 0:
		return "none (it merges cleanly into HEAD " + shortCommit(w.Target) + ")"
	default:
		return boundedReason(fmt.Sprintf("%d path(s) would conflict with HEAD %s: %s", len(w.Conflicts), shortCommit(w.Target), strings.Join(w.Conflicts, ", ")))
	}
}

// uncommittedText says what the session's uncommitted files, as the run's end found them,
// mean for taking the branch.
func (w subagentWorktree) uncommittedText() string {
	check := w.Uncommitted
	switch {
	case check.Error != "":
		return "not checked (" + check.Error + ")"
	case len(check.Overlap) == 0:
		return "the session has uncommitted changes, none in a path the branch changes"
	}
	text := fmt.Sprintf("%d path(s) the branch changes also have uncommitted changes in the session (snapshot %s), so `git merge` refuses until they are committed or stashed: %s",
		len(check.Overlap), shortCommit(check.Working), strings.Join(check.Overlap, ", "))
	if len(check.Conflicts) == 0 {
		text += "; none of them conflicts"
	} else {
		text += fmt.Sprintf("; %d of them would conflict: %s", len(check.Conflicts), strings.Join(check.Conflicts, ", "))
	}
	return boundedReason(text)
}

// startText says that the run started from the session's working state, and where its
// branch is.
func (w subagentWorktree) startText() string {
	if w.OnWorkingState {
		return fmt.Sprintf("the run started from the session's uncommitted files (snapshot %s) and its changes could not be moved onto %s without a conflict, so the branch also holds that snapshot: `git diff %s %s` shows the run's own part, `git diff %s %s | git apply --3way` applies it", shortCommit(w.Start), shortCommit(w.Base), shortCommit(w.Start), w.Branch, shortCommit(w.Start), w.Branch)
	}
	return fmt.Sprintf("the run started from the session's uncommitted files (snapshot %s); the branch holds only the run's own changes, on %s", shortCommit(w.Start), shortCommit(w.Base))
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
