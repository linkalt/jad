package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Finish collects what the run left in the worktree (message is the commit message), turns
// it into the branch and removes the worktree:
//
//  1. collect: git status, add and commit, then the run's HEAD and a pack of its new
//     objects. With a launcher these run inside the run's environment (its sandbox), so
//     nothing the run wrote is interpreted on the host; without one they run on the host
//     with the worktree's folders named outright (collectEnvironment).
//  2. import on the host: index-pack of that pack into the repository (data only), then the
//     branch is created with update-ref. For base "working" the run's changes are first
//     moved from the snapshot onto Base with merge-tree.
//  3. check the branch against the session's HEAD (merge-tree): the paths that would
//     conflict.
//  4. remove the worktree folder and its admin folder.
//
// A run that changed nothing leaves no branch. A step that fails leaves the folder in place
// (Outcome.Kept) and returns the error, so nothing the run wrote is lost.
func (t *Tree) Finish(ctx context.Context, message string, launcher Launcher) (Outcome, error) {
	outcome := Outcome{Branch: t.Branch, Base: t.Base}
	if t.Start != t.Base {
		outcome.Start = t.Start
	}
	if t.attached {
		// An earlier version's worktree commits onto its checked-out branch, which a
		// sandbox that keeps the shared git directory read-only refuses.
		launcher = nil
	}
	if t.overlay != nil {
		// Deferred first, so it runs after the mount has stopped.
		defer t.unlock()
	}
	defer t.collected()
	// From here the run's work is being collected: persisted, so that a failure or a crash
	// leaves the layers marked as waiting for another try, which nothing deletes.
	if err := t.markCollectPending(); err != nil {
		outcome.Kept = true
		return outcome, fmt.Errorf("record that the collection started: %w", err)
	}
	if t.overlay != nil && launcher == nil {
		// On the host the folder holds only the .git file: git there would see every file
		// of the run deleted.
		outcome.Kept = true
		return outcome, errors.New("an overlay worktree can only be collected inside its mount, and no launcher was given")
	}
	tip, err := t.collect(ctx, message, launcher)
	if err != nil {
		outcome.Kept = true
		return outcome, err
	}
	outcome.Commit = tip
	outcome.Changed = tip != t.Start
	if !outcome.Changed {
		outcome.Commit = t.Base
		t.collected()
		if err := t.remove(ctx, t.attached); err != nil {
			outcome.Kept = true
			return outcome, err
		}
		return outcome, nil
	}
	if !t.attached {
		if err := t.importObjects(ctx, tip, launcher); err != nil {
			outcome.Kept = true
			return outcome, err
		}
	}
	t.collected()
	branchTip, statFrom := tip, t.Base
	if t.Start != t.Base {
		if rebased, ok := t.ontoBase(ctx, tip, message); ok {
			branchTip = rebased
		} else {
			outcome.OnWorkingState, statFrom = true, t.Start
		}
	}
	if !t.attached {
		if err := t.createBranch(ctx, branchTip); err != nil {
			outcome.Kept = true
			return outcome, err
		}
	}
	outcome.Commit = branchTip
	if stat, err := run(ctx, t.options, t.common, "diff", "--shortstat", "--no-ext-diff", "--no-textconv", statFrom, branchTip); err == nil {
		outcome.Stat = strings.TrimSpace(stat)
	}
	outcome.Target, outcome.Conflicts, outcome.CheckError = t.check(ctx, branchTip)
	if outcome.CheckError == "" {
		outcome.Uncommitted = t.checkUncommitted(ctx, outcome.Target, branchTip)
	}
	if err := t.remove(ctx, false); err != nil {
		outcome.Kept = true
		return outcome, err
	}
	return outcome, nil
}

// collect commits what is left in the worktree and returns the run's HEAD.
func (t *Tree) collect(ctx context.Context, message string, launcher Launcher) (string, error) {
	command := func(flags []string, args ...string) gitCommand {
		return gitCommand{dir: t.Path, flags: flags, args: args, env: t.collectEnvironment(), launcher: launcher}
	}
	// Submodules are left alone: checking one would run git with the submodule's own
	// configuration, which the run controls.
	status, _, err := runGit(ctx, t.options, command(nil, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=all"))
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if status != "" {
		if _, _, err := runGit(ctx, t.options, command(nil, "add", "--all")); err != nil {
			return "", fmt.Errorf("git add: %w", err)
		}
		flags := hostGitFlags
		if t.options.CommitHooks {
			flags = commitHooksFlags
		}
		if launcher != nil {
			// In the run's environment a signing program (commit.gpgsign) has neither the
			// person's agent nor the network, so the commit would fail and the run's work
			// would stay in the folder. jad's own commit there is not signed; on the host it
			// is, as configured.
			flags = append(slices.Clone(flags), "-c", "commit.gpgsign=false")
		}
		arguments := append(t.identity(ctx, command), "commit", "--quiet", "--message", message)
		if !t.options.CommitHooks {
			arguments = append(arguments, "--no-verify")
		}
		if _, _, err := runGit(ctx, t.options, command(flags, arguments...)); err != nil {
			return "", fmt.Errorf("git commit: %w", err)
		}
	}
	head, _, err := runGit(ctx, t.options, command(nil, "rev-parse", "--verify", "HEAD^{commit}"))
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if !isCommitID(head) {
		return "", fmt.Errorf("the run's HEAD is not a commit name: %q", head)
	}
	return head, nil
}

// identity are the -c arguments that sign a commit with the configured author when the
// repository has no user.name or user.email of its own.
func (t *Tree) identity(ctx context.Context, command func([]string, ...string) gitCommand) []string {
	var arguments []string
	if name, _, _ := runGit(ctx, t.options, command(nil, "config", "--get", "user.name")); name == "" {
		arguments = append(arguments, "-c", "user.name="+t.options.AuthorName)
	}
	if email, _, _ := runGit(ctx, t.options, command(nil, "config", "--get", "user.email")); email == "" {
		arguments = append(arguments, "-c", "user.email="+t.options.AuthorEmail)
	}
	return arguments
}

// hostIdentity is identity for a commit jad makes in the repository on the host.
func (t *Tree) hostIdentity(ctx context.Context) []string {
	return t.identity(ctx, func(flags []string, args ...string) gitCommand {
		return gitCommand{dir: t.common, flags: flags, args: args}
	})
}

// importObjects packs the objects of tip that Start does not have (in the run's environment,
// like collect) and indexes the pack into the repository on the host, then checks that every
// object of tip is there.
func (t *Tree) importObjects(ctx context.Context, tip string, launcher Launcher) error {
	pack, err := os.CreateTemp("", "jad-run-*.pack")
	if err != nil {
		return fmt.Errorf("create the pack file: %w", err)
	}
	defer func() {
		_ = pack.Close()
		_ = os.Remove(pack.Name())
	}()
	revisions := strings.NewReader(tip + "\n^" + t.Start + "\n")
	if _, _, err := runGit(ctx, t.options, gitCommand{dir: t.Path, args: []string{"pack-objects", "--revs", "--stdout", "--thin", "--quiet"}, env: t.collectEnvironment(), stdin: revisions, stdout: pack, launcher: launcher}); err != nil {
		return fmt.Errorf("git pack-objects: %w", err)
	}
	if _, err := pack.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("read the pack file: %w", err)
	}
	if _, _, err := runGit(ctx, t.options, gitCommand{dir: t.common, args: []string{"index-pack", "--stdin", "--fix-thin"}, stdin: pack}); err != nil {
		return fmt.Errorf("git index-pack: %w", err)
	}
	if kind, err := run(ctx, t.options, t.common, "cat-file", "-t", tip); err != nil || kind != "commit" {
		return fmt.Errorf("the run's HEAD %s did not arrive in the repository as a commit (%v)", tip, err)
	}
	if _, err := run(ctx, t.options, t.common, "rev-list", "--objects", "--quiet", tip, "--not", t.Start); err != nil {
		return fmt.Errorf("the run's commits are incomplete in the repository: %w", err)
	}
	return nil
}

// ontoBase moves the run's changes from the working-state snapshot onto Base: a three-way
// merge of Base and tip with the snapshot as merge base, committed on Base. It reports false
// when that merge conflicts (the run changed lines the session had changed) or cannot run.
func (t *Tree) ontoBase(ctx context.Context, tip, message string) (string, bool) {
	output, code, err := runGit(ctx, t.options, gitCommand{dir: t.common, args: []string{"merge-tree", "--write-tree", "--no-messages", "--merge-base", t.Start, t.Base, tip}, exitCodes: []int{1}})
	if err != nil || code != 0 {
		return "", false
	}
	tree, _, _ := strings.Cut(output, "\n")
	arguments := append(t.hostIdentity(ctx), "commit-tree", tree, "-p", t.Base, "-m", message)
	commit, err := run(ctx, t.options, t.common, arguments...)
	if err != nil || !isCommitID(commit) {
		return "", false
	}
	return commit, true
}

// createBranch makes the run's branch point at commit; the branch must not exist yet.
func (t *Tree) createBranch(ctx context.Context, commit string) error {
	lock := lockFor(t.common)
	lock.Lock()
	defer lock.Unlock()
	if _, err := run(ctx, t.options, t.common, "update-ref", "refs/heads/"+t.Branch, commit, ""); err != nil {
		return fmt.Errorf("create the branch %s: %w", t.Branch, err)
	}
	return nil
}

// check returns the session's HEAD and the paths that would conflict when merging commit
// into it, or why that could not be checked.
func (t *Tree) check(ctx context.Context, commit string) (string, []string, string) {
	if t.top == "" {
		return "", nil, "the repository's checkout is not known for a worktree an earlier version made"
	}
	target, err := run(ctx, t.options, t.top, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", nil, "the session's HEAD could not be read: " + err.Error()
	}
	output, code, err := runGit(ctx, t.options, gitCommand{dir: t.common, args: []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", target, commit}, exitCodes: []int{1}})
	if err != nil {
		return target, nil, "git merge-tree failed (it needs git 2.38 or later): " + err.Error()
	}
	if code == 0 {
		return target, nil, ""
	}
	lines := strings.Split(output, "\n")
	var conflicts []string
	for _, line := range lines[1:] {
		if line != "" && !containsString(conflicts, line) {
			conflicts = append(conflicts, line)
		}
	}
	return target, conflicts, ""
}

// checkUncommitted checks commit against the session's uncommitted files (W′): a snapshot of
// them on target, the session's HEAD, as base "working" takes one. It is nil when the session
// has none.
func (t *Tree) checkUncommitted(ctx context.Context, target, commit string) *UncommittedCheck {
	working, err := workingSnapshot(ctx, t.options, t.top, target, t.Branch)
	if err != nil {
		return &UncommittedCheck{Error: "the session's uncommitted files could not be read: " + err.Error()}
	}
	if working == target {
		return nil
	}
	check := &UncommittedCheck{Working: working}
	dirty, err := t.changedPaths(ctx, target, working)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	// The branch's own changes, from where it left the session's history.
	changed, err := t.changedPaths(ctx, target+"..."+commit)
	if err != nil {
		check.Error = err.Error()
		return check
	}
	for _, path := range changed {
		if slices.Contains(dirty, path) {
			check.Overlap = append(check.Overlap, path)
		}
	}
	if len(check.Overlap) == 0 {
		return check
	}
	output, code, err := runGit(ctx, t.options, gitCommand{dir: t.common, args: []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", working, commit}, exitCodes: []int{1}})
	if err != nil {
		check.Error = "git merge-tree failed: " + err.Error()
		return check
	}
	if code != 0 {
		// The tree's name, then the conflicted paths, each ended by NUL.
		paths := strings.Split(output, "\x00")
		for _, path := range paths[1:] {
			if path != "" && !slices.Contains(check.Conflicts, path) {
				check.Conflicts = append(check.Conflicts, path)
			}
		}
	}
	return check
}

// changedPaths are the paths git diff --name-only reports for revisions, without rename
// detection, so a moved file counts as both its paths.
func (t *Tree) changedPaths(ctx context.Context, revisions ...string) ([]string, error) {
	arguments := append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z"}, revisions...)
	output, err := run(ctx, t.options, t.common, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list the changed paths: %w", err)
	}
	var paths []string
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// remove deletes the worktree folder and its admin folder (with the run's private objects),
// prunes the worktree list and, with dropBranch, deletes the branch.
func (t *Tree) remove(ctx context.Context, dropBranch bool) error {
	lock := lockFor(t.common)
	lock.Lock()
	defer lock.Unlock()
	if err := t.removeFolders(); err != nil {
		return err
	}
	if _, err := run(ctx, t.options, t.common, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w", err)
	}
	t.releaseBase(ctx)
	if dropBranch {
		if _, err := run(ctx, t.options, t.common, "branch", "--quiet", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w", err)
		}
	}
	return nil
}

// removeFolders deletes the worktree folder and, when it is where jad made it, the admin
// folder. Without git: git's own removal would read the worktree the run controlled.
func (t *Tree) removeFolders() error {
	if err := os.RemoveAll(t.Path); err != nil {
		return fmt.Errorf("remove the worktree folder: %w", err)
	}
	if t.Admin != "" && filepath.Dir(t.Admin) == filepath.Join(t.common, "worktrees") {
		if err := os.RemoveAll(t.Admin); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the worktree's admin folder: %w", err)
		}
	}
	return t.removeOverlay()
}
