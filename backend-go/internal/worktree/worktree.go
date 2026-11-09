// Package worktree gives a run its own git worktree of the session's repository and turns
// what the run changed there into a branch (docs/isolation-sandbox-design.md §5).
//
// Create checks out a commit in a new linked worktree on a detached HEAD: HEAD of the
// session's repository, a given revision, or a snapshot of the session's working state (base
// "working"). The run's own git objects go to a private folder inside the worktree's admin
// folder, with the repository's objects as a read-only alternate, so a run needs no write
// access to the shared git directory and cannot make branches or tags there. Finish commits
// what the run left (inside the run's environment when a Launcher is given), imports the run's
// objects into the repository as a pack, creates the branch, checks it against the session's
// HEAD with merge-tree, and removes the worktree; a run that changed nothing leaves no branch.
//
// With Options.Materialize overlay the worktree is not checked out: the run's files are an
// overlay of its own upper layer over a shared, read-only base checkout of the start commit
// (overlay.go, base.go), mounted by the caller in the run's holder; Finish then needs a
// Launcher that starts git inside that mount.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultDirName is the folder inside the repository's git directory that holds the
// worktrees when Options.Dir is empty. Inside .git the session's own search tools never
// list them.
const DefaultDirName = "jad-worktrees"

// ObjectsDirName is the run's private object folder inside its worktree admin folder.
const ObjectsDirName = "jad-objects"

// Bases a run's files can start from (sandbox.isolation.base).
const (
	// BaseHead starts from the repository's HEAD (or the given revision).
	BaseHead = "head"
	// BaseWorking starts from a snapshot of the session's tracked and untracked, not
	// ignored files; the branch still carries only the run's own changes.
	BaseWorking = "working"
)

// ErrNotRepository is returned by Create for a working directory outside a git repository.
var ErrNotRepository = errors.New("the working directory is not inside a git repository")

// Options configure the git commands. Every field is set by the caller.
type Options struct {
	// Git is the git executable, a name looked up in PATH or a path.
	Git string
	// Dir holds the worktrees, one folder per run; empty means DefaultDirName inside the
	// repository's common git directory.
	Dir string
	// BranchPrefix starts the name of each run's branch; the run's name follows it.
	BranchPrefix string
	// AuthorName and AuthorEmail sign the commits Finish makes when the repository has no
	// user.name or user.email of its own; a configured identity always wins.
	AuthorName  string
	AuthorEmail string
	// CommitHooks runs the repository's commit hooks for the commit of the run's changes;
	// off passes --no-verify.
	CommitHooks bool
	// Timeout bounds each git command (and, set by the caller, an overlay run's holder).
	Timeout time.Duration
	// Base is BaseHead (also when empty) or BaseWorking. A run started from a revision (a
	// resume) always starts from that revision.
	Base string
	// Materialize is MaterializeCheckout (also when empty) or MaterializeOverlay.
	Materialize string
	// CollectAttempts is how many times a caller tries to collect a run's work before it keeps
	// the folders for manual recovery. The package only carries it: Finish never gives up, and
	// nothing it keeps is deleted.
	CollectAttempts int
}

// Tree is one run's worktree.
type Tree struct {
	options Options
	top     string
	common  string
	// Path is the worktree's folder, Workspace the session's working directory inside it
	// (the same as Path when the session works at the repository's top).
	Path      string
	Workspace string
	Branch    string
	// Base is the commit the branch starts from: the repository's HEAD, or the given
	// revision. Start is the commit the files started from: Base, or the snapshot of the
	// session's working state for base "working".
	Base  string
	Start string
	// Admin is the worktree's admin folder in the repository's git directory, Objects the
	// run's private object folder inside it. Objects is empty for a worktree an earlier
	// version made, whose branch is checked out (attached).
	Admin    string
	Objects  string
	attached bool
	// overlay is set for a run of MaterializeOverlay.
	overlay *overlay
	// onCollected is called by Finish once nothing more runs in the run's mount.
	onCollected func()
}

// State is what a run records about its tree, so that Open can finish it after a restart.
type State struct {
	Path      string
	Workspace string
	Branch    string
	Base      string
	Start     string
	Admin     string
	Common    string
	Top       string
}

// State returns what Open needs to finish the tree after a restart.
func (t *Tree) State() State {
	return State{Path: t.Path, Workspace: t.Workspace, Branch: t.Branch, Base: t.Base, Start: t.Start, Admin: t.Admin, Common: t.common, Top: t.top}
}

// Timeout bounds each git command of the tree (Options.Timeout).
func (t *Tree) Timeout() time.Duration { return t.options.Timeout }

// Common is the repository's common git directory.
func (t *Tree) Common() string { return t.common }

// RunEnvironment are the variables every process of the run gets: its new objects go to the
// private folder, the repository's objects are read through an alternate, and git's automatic
// maintenance (which would write the shared git directory) stays off. Empty for a worktree of
// an earlier version.
func (t *Tree) RunEnvironment() []string {
	if t.Objects == "" {
		return nil
	}
	return []string{
		"GIT_OBJECT_DIRECTORY=" + t.Objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(t.common, "objects"),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0",
		"GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false",
	}
}

// collectEnvironment is RunEnvironment with the worktree named outright, so the commands
// that collect the run's work read neither the .git file the run could rewrite nor the
// admin folder's commondir: the repository's own configuration is the only one they read.
func (t *Tree) collectEnvironment() []string {
	return append(t.RunEnvironment(), "GIT_DIR="+t.Admin, "GIT_COMMON_DIR="+t.common, "GIT_WORK_TREE="+t.Path)
}

// Outcome is what Finish found.
type Outcome struct {
	Branch string
	Base   string
	// Commit is the branch's last commit; Changed reports whether the run changed anything.
	// A run that changed nothing has no branch left: Branch is still its name.
	Commit  string
	Changed bool
	// Stat is git's one-line summary of the change from Base, empty without one.
	Stat string
	// Kept is set when the worktree folder could not be cleaned up and is still at Path.
	Kept bool
	// Target is the session's HEAD the branch was checked against, Conflicts the paths
	// that would conflict when merging the branch into it. CheckError says why the check
	// could not run; both are empty for a run that changed nothing.
	Target     string
	Conflicts  []string
	CheckError string
	// Start is the working-state snapshot a run of base "working" started from (empty
	// otherwise). OnWorkingState is set when the run's changes could not be moved from it
	// onto Base: the branch then holds the snapshot's changes too.
	Start          string
	OnWorkingState bool
	// Uncommitted is the check against the session's uncommitted files when the run ended
	// (W′, docs/isolation-sandbox-design.md §5.3); nil when the session had none or the
	// HEAD check did not run.
	Uncommitted *UncommittedCheck
}

// UncommittedCheck is what the session's uncommitted files mean for the run's branch: Working
// is the snapshot of them (a commit on Target), Overlap the paths the branch changes that
// also have uncommitted changes (git merge refuses those until they are committed or stashed),
// Conflicts those of them a three-way merge with the snapshot conflicts on. Error says why
// the check could not run.
type UncommittedCheck struct {
	Working   string
	Overlap   []string
	Conflicts []string
	Error     string
}

// repositoryLocks serializes the commands that change a repository's worktree list and
// branches, so two runs starting together do not race on git's lock files.
var repositoryLocks sync.Map // common git dir -> *sync.Mutex

func lockFor(common string) *sync.Mutex {
	lock, _ := repositoryLocks.LoadOrStore(common, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Create adds a worktree named name for the repository that holds workspace, on a detached
// HEAD at from, or at HEAD when from is empty (or at a snapshot of the working state, for
// base "working"). The branch BranchPrefix+name is created only when the run is finished.
func Create(ctx context.Context, options Options, workspace, name, from string) (*Tree, error) {
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve the working directory: %w", err)
	}
	top, err := run(ctx, options, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotRepository, err)
	}
	common, err := run(ctx, options, workspace, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotRepository, err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workspace, common)
	}
	if common, err = filepath.EvalSymlinks(common); err != nil {
		return nil, fmt.Errorf("resolve the git directory: %w", err)
	}
	if top, err = filepath.EvalSymlinks(top); err != nil {
		return nil, fmt.Errorf("resolve the repository folder: %w", err)
	}
	revision := from
	if revision == "" {
		revision = "HEAD"
	}
	base, err := run(ctx, options, top, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil {
		if from == "" {
			return nil, errors.New("the repository has no commit yet, so there is nothing to start a worktree from")
		}
		return nil, fmt.Errorf("revision %s does not name a commit", from)
	}
	relative, err := filepath.Rel(top, workspace)
	if err != nil || strings.HasPrefix(relative, "..") {
		return nil, fmt.Errorf("the working directory %s is not inside the repository %s", workspace, top)
	}
	start := base
	if options.Base == BaseWorking && from == "" {
		if start, err = workingSnapshot(ctx, options, top, base, name); err != nil {
			return nil, fmt.Errorf("snapshot the working state: %w", err)
		}
	}
	dir := options.Dir
	if dir == "" {
		dir = filepath.Join(common, DefaultDirName)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the worktree folder: %w", err)
	}
	tree := &Tree{options: options, top: top, common: common, Path: filepath.Join(dir, name), Branch: options.BranchPrefix + name, Base: base, Start: start}
	tree.Workspace = filepath.Join(tree.Path, relative)
	switch options.Materialize {
	case "", MaterializeCheckout:
	case MaterializeOverlay:
		if err := tree.addOverlay(ctx, relative); err != nil {
			return nil, err
		}
		return tree, nil
	default:
		return nil, fmt.Errorf("unknown materialiser %q", options.Materialize)
	}
	if err := tree.add(ctx); err != nil {
		return nil, err
	}
	if _, err := os.Stat(tree.Workspace); err != nil {
		// The session's folder is not in the commit (it is new or ignored): the run gets
		// the folder empty rather than not at all.
		if err := os.MkdirAll(tree.Workspace, 0o755); err != nil {
			_ = tree.remove(ctx, false)
			return nil, fmt.Errorf("create the working directory in the worktree: %w", err)
		}
	}
	return tree, nil
}

// add checks Start out at Path on a detached HEAD and makes the private object folder. It
// runs under the repository's lock; the branch name must be free, since Finish creates it.
func (t *Tree) add(ctx context.Context) error {
	lock := lockFor(t.common)
	lock.Lock()
	defer lock.Unlock()
	if BranchExists(ctx, t.options, t.top, t.Branch) {
		return fmt.Errorf("the branch %s already exists", t.Branch)
	}
	if _, err := run(ctx, t.options, t.top, "worktree", "add", "--quiet", "--detach", t.Path, t.Start); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	admin, err := run(ctx, t.options, t.Path, "rev-parse", "--absolute-git-dir")
	if err == nil {
		admin, err = filepath.EvalSymlinks(admin)
	}
	if err == nil && filepath.Dir(admin) != filepath.Join(t.common, "worktrees") {
		err = fmt.Errorf("git placed it at %s, outside %s", admin, filepath.Join(t.common, "worktrees"))
	}
	if err != nil {
		_ = os.RemoveAll(t.Path)
		_, _ = run(ctx, t.options, t.top, "worktree", "prune")
		return fmt.Errorf("find the worktree's admin folder: %w", err)
	}
	t.Admin, t.Objects = admin, filepath.Join(admin, ObjectsDirName)
	if err := os.Mkdir(t.Objects, 0o755); err != nil {
		t.removeFolders()
		_, _ = run(ctx, t.options, t.top, "worktree", "prune")
		return fmt.Errorf("create the run's object folder: %w", err)
	}
	return nil
}

// ErrGone is returned by Open when the worktree folder no longer exists.
var ErrGone = errors.New("the worktree folder is gone")

// Open is the Tree of a worktree an earlier server created, so that a run the server lost
// when it stopped can still be finished: Finish commits what the run left there and removes
// the folder like at the end of a run. A state without Admin (an earlier version's record)
// finds the git folders the way that version did, from the worktree itself.
func Open(ctx context.Context, options Options, state State) (*Tree, error) {
	if info, err := os.Stat(state.Path); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrGone, state.Path)
	}
	tree := &Tree{options: options, top: state.Top, common: state.Common, Path: state.Path, Workspace: state.Workspace, Branch: state.Branch, Base: state.Base, Start: state.Start, Admin: state.Admin}
	if tree.Workspace == "" {
		tree.Workspace = tree.Path
	}
	if tree.Start == "" {
		tree.Start = tree.Base
	}
	if tree.Admin != "" && tree.common != "" {
		tree.Objects = filepath.Join(tree.Admin, ObjectsDirName)
		if err := tree.openOverlay(); err != nil {
			return nil, err
		}
		return tree, nil
	}
	common, err := run(ctx, options, state.Path, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotRepository, err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(state.Path, common)
	}
	if tree.common, err = filepath.EvalSymlinks(common); err != nil {
		return nil, fmt.Errorf("resolve the git directory: %w", err)
	}
	if tree.Admin, err = run(ctx, options, state.Path, "rev-parse", "--absolute-git-dir"); err != nil {
		return nil, fmt.Errorf("find the worktree's admin folder: %w", err)
	}
	if info, err := os.Stat(filepath.Join(tree.Admin, ObjectsDirName)); err == nil && info.IsDir() {
		tree.Objects = filepath.Join(tree.Admin, ObjectsDirName)
	}
	_, err = run(ctx, options, state.Path, "symbolic-ref", "--quiet", "HEAD")
	tree.attached = err == nil
	return tree, nil
}

// BranchExists reports whether the repository that holds workspace has the branch.
func BranchExists(ctx context.Context, options Options, workspace, branch string) bool {
	_, err := run(ctx, options, workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}
