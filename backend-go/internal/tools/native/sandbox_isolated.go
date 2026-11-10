package native

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// isolatedRun is what jad knows about the git folders of a run in a worktree of its own,
// from when it made them (docs/isolation-sandbox-design.md §9.3 step 6). The sandbox of every
// call of the run takes them from here, never from the worktree's .git file or the admin
// folder's commondir, which the run can rewrite.
type isolatedRun struct {
	path   string
	common string
	admin  string
	// env are the variables every sandboxed process of the run gets (Tree.RunEnvironment).
	env []string
	// holder is the running holder of an overlay run, whose namespaces every process of the
	// run joins; nil for a checkout, and while an overlay run has none (sandbox_holder.go).
	holder *sandboxHolder
	// mcp is the MCP servers of the run's own, once openRunMCP made the route to them; nil
	// for a run without (a test, or a recovery).
	mcp *runMCP
}

// isolatedRuns are the runs in a worktree of their own that are going, by worktree folder.
var isolatedRuns = struct {
	sync.RWMutex
	byPath map[string]isolatedRun
}{byPath: map[string]isolatedRun{}}

// registerIsolatedRun records tree's folders until forgetIsolatedRun.
func registerIsolatedRun(tree *worktree.Tree) {
	run := isolatedRun{path: filepath.Clean(tree.Path), common: tree.Common(), admin: tree.Admin, env: tree.RunEnvironment()}
	isolatedRuns.Lock()
	isolatedRuns.byPath[run.path] = run
	isolatedRuns.Unlock()
}

// forgetIsolatedRun drops what registerIsolatedRun recorded for tree, and stops its holder.
func forgetIsolatedRun(tree *worktree.Tree) {
	isolatedRuns.Lock()
	path := filepath.Clean(tree.Path)
	holder := isolatedRuns.byPath[path].holder
	mcp := isolatedRuns.byPath[path].mcp
	delete(isolatedRuns.byPath, path)
	isolatedRuns.Unlock()
	// The run's servers go before its holder: a server joined to an overlay run is stopped
	// as a process first, not as a side effect of the mount going away.
	if mcp != nil {
		mcp.end()
	}
	holder.stop()
}

// attachHolder makes holder the one every process of tree's run joins (registering the run
// when it is not yet).
func attachHolder(tree *worktree.Tree, holder *sandboxHolder) {
	path := filepath.Clean(tree.Path)
	isolatedRuns.Lock()
	run, ok := isolatedRuns.byPath[path]
	if !ok {
		run = isolatedRun{path: path, common: tree.Common(), admin: tree.Admin, env: tree.RunEnvironment()}
	}
	previous := run.holder
	run.holder = holder
	isolatedRuns.byPath[path] = run
	isolatedRuns.Unlock()
	if previous != holder {
		previous.stop()
	}
	recordHolder(tree, holder)
}

// recordHolder writes the holder's identity (pid and start time) into the overlay run's state
// file, or clears it for nil, so a later server finds a holder that outlived this one
// (reapStaleHolder). A run that cannot record it still runs; the failure is logged.
func recordHolder(tree *worktree.Tree, holder *sandboxHolder) {
	var ref worktree.HolderRef
	if holder != nil {
		ref = worktree.HolderRef{PID: holder.identity.pid, Start: holder.identity.start}
	}
	if err := tree.SetHolder(ref); err != nil {
		slog.Default().Warn("the overlay run's holder could not be recorded in its state file", "run", tree.Path, "error", err)
	}
}

// detachHolder stops the holder of tree's run, if it has one; the run stays registered.
func detachHolder(tree *worktree.Tree) {
	path := filepath.Clean(tree.Path)
	isolatedRuns.Lock()
	run, ok := isolatedRuns.byPath[path]
	holder := run.holder
	if ok {
		run.holder = nil
		isolatedRuns.byPath[path] = run
	}
	isolatedRuns.Unlock()
	holder.stop()
	if holder != nil {
		recordHolder(tree, nil)
	}
}

// holderOf is the pid of the holder of the overlay run whose worktree holds dir; 0 for any
// other folder.
func holderOf(dir string) int {
	if holder := holderFor(dir); holder != nil {
		return holder.pid
	}
	return 0
}

// holderFor is the holder of the overlay run whose worktree holds dir; nil for any other
// folder.
func holderFor(dir string) *sandboxHolder {
	if run, ok := isolatedRunOf(dir); ok {
		return run.holder
	}
	return nil
}

// isolatedRunOf is the isolated run whose worktree holds dir.
func isolatedRunOf(dir string) (isolatedRun, bool) {
	if dir == "" {
		return isolatedRun{}, false
	}
	candidates := []string{filepath.Clean(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != candidates[0] {
		candidates = append(candidates, resolved)
	}
	isolatedRuns.RLock()
	defer isolatedRuns.RUnlock()
	for _, candidate := range candidates {
		for path, run := range isolatedRuns.byPath {
			if pathInside(candidate, path) {
				return run, true
			}
		}
	}
	return isolatedRun{}, false
}

// sandboxWritePaths are a sandbox's writable paths for place. In an isolated run
// {git_common_dir} stands for the run's own admin folder (with its private objects), not for
// the repository's git directory: the run writes its detached HEAD, index and objects there
// and nothing shared, so it can make no branch or tag and cannot touch another run's state. A
// path below {git_common_dir} is kept only when it lies inside that admin folder.
func sandboxWritePaths(paths []string, place sandboxPlace) []string {
	run, ok := isolatedRunOf(place.workspace)
	if !ok {
		return paths
	}
	narrowed := make([]string, 0, len(paths))
	for _, path := range paths {
		rest, found := strings.CutPrefix(path, "{git_common_dir}")
		switch {
		case !found:
			narrowed = append(narrowed, path)
		case rest == "":
			narrowed = append(narrowed, run.admin)
		default:
			if full := filepath.Join(run.common, rest); pathInside(full, run.admin) {
				narrowed = append(narrowed, full)
			}
		}
	}
	return narrowed
}

// sandboxRunEnvironment are the variables a sandboxed child of place gets besides TMPDIR.
func sandboxRunEnvironment(place sandboxPlace) []string {
	if run, ok := isolatedRunOf(place.workspace); ok {
		return run.env
	}
	return nil
}

// collectLauncher starts the git commands that collect an isolated run's work inside the
// run's own sandbox (docs/isolation-sandbox-design.md §3 step 8), so that nothing the run left
// is interpreted on the host. The sandbox is the run's, with the run's admin folder writable
// (git add and commit write the index, HEAD and objects there even when the run's own write
// paths leave it out). Nil without a sandbox: the commands then run on the host, which the
// run could reach as well; an overlay run's still start in its holder (holderLauncher).
func collectLauncher(execution tools.ExecutionContext, tree *worktree.Tree) worktree.Launcher {
	if execution.Environment == nil || execution.Environment.Sandbox == nil {
		if _, overlay := tree.Layers(); overlay {
			// Without a sandbox an overlay run is still collected inside its mount.
			return holderLauncher(tree)
		}
		return nil
	}
	sandbox := *execution.Environment.Sandbox
	sandbox.WritablePaths = append(slices.Clone(sandbox.WritablePaths), tree.Admin)
	execution.PlanMode = false
	return func(git string, args []string, dir string) (worktree.Launch, error) {
		resolved, err := exec.LookPath(git)
		if err != nil {
			return worktree.Launch{}, err
		}
		tmp, err := callTempDir(&sandbox, execution)
		if err != nil {
			return worktree.Launch{}, err
		}
		launch, err := sandboxed(&sandbox, sandboxPlace{workspace: execution.WorkspaceRoot, cwd: dir, tmp: tmp, sessionDir: execution.SessionDir}, git, resolved, args)
		if err != nil {
			return worktree.Launch{}, err
		}
		return worktree.Launch{File: launch.resolved, Argv0: launch.configured, Args: launch.args, Env: buildNativeBashEnvironment(os.Environ(), sessionfs.NativeBashSettings{}, "")}, nil
	}
}

// holderLauncher starts git in the holder of tree's overlay run without a sandbox, with the
// host's environment as a collection on the host has.
func holderLauncher(tree *worktree.Tree) worktree.Launcher {
	return func(git string, args []string, dir string) (worktree.Launch, error) {
		resolved, err := exec.LookPath(git)
		if err != nil {
			return worktree.Launch{}, err
		}
		holder := holderFor(tree.Workspace)
		if holder == nil {
			return worktree.Launch{}, errors.New("the overlay run has no holder to collect in")
		}
		launch, err := joinHolder(holder, dir, sandboxLaunch{configured: git, resolved: resolved, args: args})
		if err != nil {
			return worktree.Launch{}, err
		}
		return worktree.Launch{File: launch.resolved, Argv0: launch.configured, Args: launch.args, Env: os.Environ()}, nil
	}
}
