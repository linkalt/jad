// Package worktree gives a run its own git worktree of the session's repository and turns
// what the run changed there into a branch.
//
// Create adds a worktree on a new branch at the repository's HEAD (or at a given revision);
// Finish commits whatever the run left in it, removes the folder and keeps the branch when
// the run changed something, or drops the branch too when it changed nothing. Uncommitted
// changes of the session's own checkout are not in a new worktree: it starts from a commit.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultDirName is the folder inside the repository's git directory that holds the
// worktrees when Options.Dir is empty. Inside .git the session's own search tools never
// list them.
const DefaultDirName = "jad-worktrees"

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
	// AuthorName and AuthorEmail sign the commit Finish makes when the repository has no
	// user.name or user.email of its own; a configured identity always wins.
	AuthorName  string
	AuthorEmail string
	// CommitHooks runs the repository's commit hooks for that commit; off passes --no-verify.
	CommitHooks bool
	// Timeout bounds each git command.
	Timeout time.Duration
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
	// Base is the commit the worktree started from.
	Base string
}

// Outcome is what Finish found.
type Outcome struct {
	Branch string
	Base   string
	// Commit is the branch's last commit; Changed reports whether it differs from Base.
	// A run that changed nothing has no branch left: Branch is still its name.
	Commit  string
	Changed bool
	// Stat is git's one-line summary of the change from Base, empty without one.
	Stat string
	// Kept is set when the worktree folder could not be cleaned up and is still at Path.
	Kept bool
}

// repositoryLocks serializes the commands that change a repository's worktree list and
// branches, so two runs starting together do not race on git's lock files.
var repositoryLocks sync.Map // common git dir -> *sync.Mutex

func lockFor(common string) *sync.Mutex {
	lock, _ := repositoryLocks.LoadOrStore(common, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Create adds a worktree named name for the repository that holds workspace. The branch is
// BranchPrefix+name and starts at from, or at HEAD when from is empty.
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
	dir := options.Dir
	if dir == "" {
		dir = filepath.Join(common, DefaultDirName)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the worktree folder: %w", err)
	}
	tree := &Tree{options: options, top: top, common: common, Path: filepath.Join(dir, name), Branch: options.BranchPrefix + name, Base: base}
	tree.Workspace = filepath.Join(tree.Path, relative)
	lock := lockFor(common)
	lock.Lock()
	defer lock.Unlock()
	if _, err := run(ctx, options, top, "worktree", "add", "--quiet", "-b", tree.Branch, tree.Path, base); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	if _, err := os.Stat(tree.Workspace); err != nil {
		// The session's folder is not in the commit (it is new or ignored): the run gets
		// the folder empty rather than not at all.
		if err := os.MkdirAll(tree.Workspace, 0o755); err != nil {
			tree.remove(ctx, true)
			return nil, fmt.Errorf("create the working directory in the worktree: %w", err)
		}
	}
	return tree, nil
}

// Finish commits what the run left in the worktree (message is the commit message), then
// removes the worktree folder. The branch stays when it has commits the base has not, and
// is deleted otherwise. A commit that fails leaves the folder in place (Outcome.Kept) and
// returns the error, so nothing the run wrote is lost.
func (t *Tree) Finish(ctx context.Context, message string) (Outcome, error) {
	outcome := Outcome{Branch: t.Branch, Base: t.Base}
	status, err := run(ctx, t.options, t.Path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		outcome.Kept = true
		return outcome, fmt.Errorf("git status: %w", err)
	}
	if status != "" {
		if _, err := run(ctx, t.options, t.Path, "add", "--all"); err != nil {
			outcome.Kept = true
			return outcome, fmt.Errorf("git add: %w", err)
		}
		if _, err := run(ctx, t.options, t.Path, t.commitArguments(ctx, message)...); err != nil {
			outcome.Kept = true
			return outcome, fmt.Errorf("git commit: %w", err)
		}
	}
	head, err := run(ctx, t.options, t.Path, "rev-parse", "HEAD")
	if err != nil {
		outcome.Kept = true
		return outcome, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	outcome.Commit = head
	outcome.Changed = head != t.Base
	if outcome.Changed {
		if stat, err := run(ctx, t.options, t.Path, "diff", "--shortstat", t.Base, head); err == nil {
			outcome.Stat = strings.TrimSpace(stat)
		}
	}
	if err := t.remove(ctx, !outcome.Changed); err != nil {
		outcome.Kept = true
		return outcome, err
	}
	return outcome, nil
}

// commitArguments are the arguments of the commit Finish makes: the configured author
// only when the repository has no identity, and --no-verify unless hooks are wanted.
func (t *Tree) commitArguments(ctx context.Context, message string) []string {
	var arguments []string
	if name, _ := run(ctx, t.options, t.Path, "config", "--get", "user.name"); name == "" {
		arguments = append(arguments, "-c", "user.name="+t.options.AuthorName)
	}
	if email, _ := run(ctx, t.options, t.Path, "config", "--get", "user.email"); email == "" {
		arguments = append(arguments, "-c", "user.email="+t.options.AuthorEmail)
	}
	arguments = append(arguments, "commit", "--quiet", "--message", message)
	if !t.options.CommitHooks {
		arguments = append(arguments, "--no-verify")
	}
	return arguments
}

// remove deletes the worktree folder and, with dropBranch, the branch.
func (t *Tree) remove(ctx context.Context, dropBranch bool) error {
	lock := lockFor(t.common)
	lock.Lock()
	defer lock.Unlock()
	if _, err := run(ctx, t.options, t.top, "worktree", "remove", "--force", t.Path); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	if dropBranch {
		if _, err := run(ctx, t.options, t.top, "branch", "--quiet", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w", err)
		}
	}
	return nil
}

// BranchExists reports whether the repository that holds workspace has the branch.
func BranchExists(ctx context.Context, options Options, workspace, branch string) bool {
	_, err := run(ctx, options, workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// run runs git in dir and returns its standard output without the trailing newline. A
// failure carries the first line of git's error output.
func run(ctx context.Context, options Options, dir string, arguments ...string) (string, error) {
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, options.Git, arguments...)
	command.Dir = dir
	// A command never waits for a password, and the messages stay parseable.
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if line, _, found := strings.Cut(message, "\n"); found {
			message = line
		}
		if message == "" {
			return "", err
		}
		return "", fmt.Errorf("%v: %s", err, message)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}
