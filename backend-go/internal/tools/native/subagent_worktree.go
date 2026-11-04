package native

import (
	"context"
	"fmt"
	"path/filepath"
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
	// Commit is the branch's last commit once the run ended; Changed reports whether it
	// differs from Base. A run that changed nothing has no branch left.
	Commit  string `json:"commit,omitempty"`
	Changed bool   `json:"changed,omitempty"`
	Stat    string `json:"stat,omitempty"`
	// Path is the worktree folder while the run goes, and after its end only when the
	// folder could not be cleaned up (Error says why).
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
	// Note says why the worktree started where it did, when that is not what was asked.
	Note string `json:"note,omitempty"`
}

// subagentWorktreeStart asks the manager to run the child in a worktree of its own.
type subagentWorktreeStart struct {
	options worktree.Options
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
	}
}

// openWorktree creates the run's worktree and points the child's tools at it: the
// workspace root becomes the session's folder inside the worktree, the session's own
// folder becomes the origin its path rules are moved from, and the agent's path scope is
// moved the same way.
func (m *SubagentManager) openWorktree(ctx context.Context, run *subagentRun, spec *subagentStart) (*worktree.Tree, error) {
	execution := &spec.request.Execution
	parentRoot := execution.WorkspaceRoot
	record := subagentWorktree{}
	from := spec.worktree.from
	if from != "" && !worktree.BranchExists(ctx, spec.worktree.options, parentRoot, from) {
		record.Note = fmt.Sprintf("the branch %s of the resumed run is gone, so this run starts from HEAD", from)
		from = ""
	}
	tree, err := worktree.Create(ctx, spec.worktree.options, parentRoot, run.id, from)
	if err != nil {
		return nil, err
	}
	if execution.WorkspaceOrigin == "" {
		execution.WorkspaceOrigin = parentRoot
	}
	execution.WorkspaceRoot = tree.Workspace
	execution.PathScope = movedScope(execution.PathScope, parentRoot, tree.Workspace)
	record.Branch, record.Base, record.Path = tree.Branch, tree.Base, tree.Path
	run.setWorktree(record)
	return tree, nil
}

// closeWorktree commits what the child left in its worktree and removes the folder; the
// branch stays when the run changed something. It runs after the child and its processes
// have stopped, and also when the run was stopped or the server is shutting down, so the
// work is never left behind in a folder nobody knows of. Each git command is bounded by
// the worktree timeout.
func (m *SubagentManager) closeWorktree(run *subagentRun, tree *worktree.Tree) {
	view := run.view()
	message := fmt.Sprintf("jad sub-agent %s (run %s)\n\n%s", view.agent, view.id, view.task)
	outcome, err := tree.Finish(context.WithoutCancel(run.ctx), message)
	run.mu.Lock()
	record := subagentWorktree{Branch: outcome.Branch, Base: outcome.Base, Commit: outcome.Commit, Changed: outcome.Changed, Stat: outcome.Stat}
	if run.worktree != nil {
		record.Note = run.worktree.Note
	}
	if err != nil {
		record.Error = boundedReason(err.Error())
	}
	if outcome.Kept {
		record.Path = tree.Path
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
			tools.Information{Key: "worktree_next", Value: fmt.Sprintf("the session's own files are unchanged; review with `git diff %s %s`, take the work with `git merge %s` (or cherry-pick), or drop it with `git branch -D %s`", shortCommit(record.Base), record.Branch, record.Branch, record.Branch)})
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
		text = fmt.Sprintf("worktree branch %s (from %s): %s. The session's files are unchanged: review with `git diff %s %s`, take the work with `git merge %s`, drop it with `git branch -D %s`",
			record.Branch, shortCommit(record.Base), record.Stat, shortCommit(record.Base), record.Branch, record.Branch, record.Branch)
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

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
