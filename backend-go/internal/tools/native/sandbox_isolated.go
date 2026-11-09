package native

import (
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

// forgetIsolatedRun drops what registerIsolatedRun recorded for tree.
func forgetIsolatedRun(tree *worktree.Tree) {
	isolatedRuns.Lock()
	delete(isolatedRuns.byPath, filepath.Clean(tree.Path))
	isolatedRuns.Unlock()
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
// run could reach as well.
func collectLauncher(execution tools.ExecutionContext, tree *worktree.Tree) worktree.Launcher {
	if execution.Environment == nil || execution.Environment.Sandbox == nil {
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
