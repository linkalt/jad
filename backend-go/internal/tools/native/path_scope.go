package native

import (
	"os"
	"path/filepath"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// scopedPolicy returns the tool's policy narrowed by the call's PathScope. The scope
// adds a condition (Within) and extra deny rules; it never removes a rule, so the
// result allows a subset of what policy allowed. It is applied once, to a policy that
// has no Within of its own: only this function sets it.
func scopedPolicy(policy sessionfs.NativePathPolicy, execution tools.ExecutionContext) sessionfs.NativePathPolicy {
	return environmentPolicy(pathScopePolicy(workspacePolicy(policy, execution), execution), execution)
}

// pathScopePolicy narrows policy by the call's PathScope.
func pathScopePolicy(policy sessionfs.NativePathPolicy, execution tools.ExecutionContext) sessionfs.NativePathPolicy {
	scope := execution.PathScope
	if scope == nil {
		return policy
	}
	root := execution.WorkspaceRoot
	narrowed := sessionfs.NativePathPolicy{
		Paths:             append([]sessionfs.NativePathRule(nil), policy.Paths...),
		UnrestrictedPaths: policy.UnrestrictedPaths,
		Within:            append([]string(nil), policy.Within...),
	}
	if len(scope.Allow) > 0 {
		allowed := make([]string, 0, len(scope.Allow))
		for _, entry := range scope.Allow {
			allowed = append(allowed, scopeRoot(root, entry))
		}
		narrowed.Within = allowed
	}
	for _, entry := range scope.Deny {
		narrowed.Paths = append(narrowed.Paths, sessionfs.NativePathRule{Action: "deny", Path: scopeRoot(root, entry)})
	}
	return narrowed
}

// scopeRoot makes one entry absolute from the workspace root and follows symlinks of
// the part that exists, because the path checks compare against resolved paths.
func scopeRoot(workspaceRoot, entry string) string {
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(workspaceRoot, entry)
	}
	entry = filepath.Clean(entry)
	existing, rest := entry, ""
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return entry
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// workspacePolicy is the tool's policy for a call that runs in a git worktree of the
// session's working directory (ExecutionContext.WorkspaceOrigin): a rule on a path inside
// the origin applies to the same path inside the worktree instead. The rule no longer
// reaches the origin, so an allow rule of the session does not let the worktree run change
// the session's own checkout. Rules outside the origin are kept as they are. Without an
// origin the policy is returned unchanged.
func workspacePolicy(policy sessionfs.NativePathPolicy, execution tools.ExecutionContext) sessionfs.NativePathPolicy {
	origin, root := execution.WorkspaceOrigin, execution.WorkspaceRoot
	if origin == "" || root == "" || filepath.Clean(origin) == filepath.Clean(root) {
		return policy
	}
	origins := []string{filepath.Clean(origin)}
	if canonical := sessionfs.CanonicalRulePath(origin); canonical != origins[0] {
		origins = append(origins, canonical)
	}
	moved := sessionfs.NativePathPolicy{
		Paths:             make([]sessionfs.NativePathRule, 0, len(policy.Paths)),
		UnrestrictedPaths: policy.UnrestrictedPaths,
		Within:            make([]string, 0, len(policy.Within)),
	}
	for _, rule := range policy.Paths {
		moved.Paths = append(moved.Paths, sessionfs.NativePathRule{Action: rule.Action, Path: movePath(origins, root, rule.Path)})
	}
	for _, entry := range policy.Within {
		moved.Within = append(moved.Within, movePath(origins, root, entry))
	}
	return moved
}

// movePath puts path, when it lies inside one of origins, at the same place under root.
func movePath(origins []string, root, path string) string {
	for _, origin := range origins {
		if !sessionfs.PathWithin(origin, path) {
			continue
		}
		if relative, err := filepath.Rel(origin, filepath.Clean(path)); err == nil {
			return filepath.Join(root, relative)
		}
	}
	return path
}

func withinAny(roots []string, target string) bool {
	for _, root := range roots {
		if sessionfs.PathWithin(root, target) {
			return true
		}
	}
	return false
}

// narrowScope is the scope of a child: its agent's own paths cut down to what the
// parent's scope reaches, so a child never reaches more than the run that started it.
// Allow is the intersection of both Allow lists (an empty list reaches everything), Deny
// is both Deny lists together. An intersection with nothing in it denies every path.
func narrowScope(workspaceRoot string, parent *tools.PathScope, own *tools.PathScope) *tools.PathScope {
	if parent == nil && own == nil {
		return nil
	}
	if parent == nil {
		return &tools.PathScope{Allow: append([]string(nil), own.Allow...), Deny: append([]string(nil), own.Deny...)}
	}
	if own == nil {
		own = &tools.PathScope{}
	}
	narrowed := &tools.PathScope{}
	for _, entry := range append(append([]string(nil), parent.Deny...), own.Deny...) {
		narrowed.Deny = append(narrowed.Deny, scopeRoot(workspaceRoot, entry))
	}
	switch {
	case len(parent.Allow) == 0:
		for _, entry := range own.Allow {
			narrowed.Allow = append(narrowed.Allow, scopeRoot(workspaceRoot, entry))
		}
	case len(own.Allow) == 0:
		for _, entry := range parent.Allow {
			narrowed.Allow = append(narrowed.Allow, scopeRoot(workspaceRoot, entry))
		}
	default:
		for _, a := range parent.Allow {
			a = scopeRoot(workspaceRoot, a)
			for _, b := range own.Allow {
				b = scopeRoot(workspaceRoot, b)
				switch {
				case sessionfs.PathWithin(a, b):
					narrowed.Allow = append(narrowed.Allow, b)
				case sessionfs.PathWithin(b, a):
					narrowed.Allow = append(narrowed.Allow, a)
				}
			}
		}
		if len(narrowed.Allow) == 0 {
			narrowed.Deny = append(narrowed.Deny, string(filepath.Separator))
		}
	}
	return narrowed
}

// environmentPolicy narrows policy to what the run's sandbox shows (docs/isolation-sandbox-design.md
// §2.4, §9.3): the in-process readers (code_context, read_image, the lsp tool's file view,
// transcript) have no child to put in a sandbox, so they are given the environment's own sets
// and answer as if the path were absent. Each hidden path, the {tmp} folders and the worktrees
// of other runs are denied; with read paths, only what they and the writable paths show is
// within reach. Without an environment the policy is returned as it is.
func environmentPolicy(policy sessionfs.NativePathPolicy, execution tools.ExecutionContext) sessionfs.NativePathPolicy {
	if execution.Environment == nil || execution.Environment.Sandbox == nil {
		return policy
	}
	sandbox := execution.Environment.Sandbox
	if execution.PlanMode {
		sandbox = sandbox.ForPlanMode()
	}
	root := execution.WorkspaceRoot
	place := sandboxPlace{workspace: root, cwd: root, sessionDir: execution.SessionDir}
	expand := sandboxExpander(place)
	mirrored := sessionfs.NativePathPolicy{
		Paths:             append([]sessionfs.NativePathRule(nil), policy.Paths...),
		UnrestrictedPaths: policy.UnrestrictedPaths,
		Within:            append([]string(nil), policy.Within...),
	}
	deny := func(path string) {
		if path != "" {
			mirrored.Paths = append(mirrored.Paths, sessionfs.NativePathRule{Action: "deny", Path: scopeRoot(root, path)})
		}
	}
	for _, path := range sandbox.HidePaths {
		deny(expand(path))
	}
	deny(sandboxTempRoot())
	for _, path := range otherRunsWorktrees(root) {
		deny(path)
	}
	if sandbox.WholeFileSystemReadable() {
		return mirrored
	}
	writable := sandboxWritableRoots(sandboxWritePaths(sandbox.WritablePaths, place), expand)
	reach := append([]string(nil), writable...)
	for _, read := range sandboxReadRoots(sandbox.ReadPaths, expand, writable) {
		reach = append(reach, read.real)
		if read.link != "" {
			reach = append(reach, read.link)
		}
	}
	if len(mirrored.Within) == 0 {
		mirrored.Within = reach
		if len(reach) == 0 {
			mirrored.Paths = append(mirrored.Paths, sessionfs.NativePathRule{Action: "deny", Path: string(filepath.Separator)})
		}
		return mirrored
	}
	var within []string
	for _, a := range mirrored.Within {
		for _, b := range reach {
			switch {
			case sessionfs.PathWithin(b, a):
				within = append(within, a)
			case sessionfs.PathWithin(a, b):
				within = append(within, b)
			}
		}
	}
	mirrored.Within = within
	if len(within) == 0 {
		mirrored.Paths = append(mirrored.Paths, sessionfs.NativePathRule{Action: "deny", Path: string(filepath.Separator)})
	}
	return mirrored
}

// otherRunsWorktrees are the worktree checkouts and admin folders of runs other than the one
// working in workspace, which no sandbox of that run shows (S4).
func otherRunsWorktrees(workspace string) []string {
	common := gitCommonDir(workspace)
	if common == "" {
		return nil
	}
	common, err := filepath.EvalSymlinks(common)
	if err != nil {
		return nil
	}
	own := workspaceGitDir(workspace, common)
	ownWorkspace, _ := filepath.EvalSymlinks(workspace)
	var others []string
	for _, folder := range []string{filepath.Join(common, "worktrees"), filepath.Join(common, worktree.DefaultDirName)} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(folder, entry.Name())
			if path == own || pathInside(ownWorkspace, path) {
				continue
			}
			others = append(others, path)
		}
	}
	return others
}
