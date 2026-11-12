package native

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/worktree"
)

// The fixed rules of every bwrap and seatbelt sandbox, whatever the sandbox setting lists
// (docs/isolation-sandbox-design.md §7.2, §9.3 step 5). They only take away: none makes a
// path writable or visible that the setting's own paths do not already cover.
//
//   - S1/S2: the repository's git directory is pinned (bound onto itself, so it cannot be
//     renamed away and replaced), and its hooks folder and config files are read-only, so a
//     sandboxed call cannot plant a hook or an fsmonitor command for the git that runs outside.
//   - S3: {tmp} is the call's own folder under sandboxTempRoot; the root is hidden, so a call
//     cannot read another session's code and result files or feed another session's waiting
//     interpreter through its code pipe (a named pipe stays writable on a read-only mount).
//   - S4: the worktrees of other runs (<git dir>/worktrees, <git dir>/jad-worktrees) are
//     hidden; a run in a worktree keeps its own.
//   - S5: a writable path whose name a sandboxed call could have changed (it lies in a folder
//     another writable path covers) is not bound on its own: the folder's bind covers what is
//     really there, and a symlink the call put in its place cannot point the bind elsewhere.
//   - D27/D28: the session's own configuration (sandbox templates, the session file, the
//     catalogs, the root setting, the agent folders and the state folder with the transcripts)
//     stays read-only under a writable path, so a call cannot widen the next one's environment
//     or rewrite a transcript; a writable path inside one of them is writable again, and the
//     session's own state folder is made read-only last, over any writable path that holds it.

// sandboxTempDirName starts the name of the folder in the system temp folder that holds every
// session's {tmp}; the user's id follows it, so users of one machine never share it.
const sandboxTempDirName = "jad-sandbox-"

// sandboxTempIDBytes is how much of the digest names one {tmp} folder.
const sandboxTempIDBytes = 8

// sandboxTempParent is the folder sandboxTempRoot is made in: the system temp folder. It is a
// variable so that the tests keep their {tmp} folders apart from the machine's.
var sandboxTempParent = os.TempDir

// sandboxTempRoot is the folder that holds the {tmp} folders: in the system temp folder, or
// in the short tool_smith fallback when a bridge socket in a {tmp} folder there would not fit
// in a unix socket path (makeToolSmithWorkDir).
func sandboxTempRoot() string {
	name := sandboxTempDirName + strconv.Itoa(os.Getuid())
	root := filepath.Join(sandboxTempParent(), name)
	deepest := filepath.Join(root, strings.Repeat("0", 2*sandboxTempIDBytes), "tool-smith-0000000000", toolSmithSocketName)
	if len(deepest) > toolSmithMaxSocketPath {
		return filepath.Join(toolSmithShortTempDir, name)
	}
	return root
}

// ensurePrivateDir makes dir (mode 0700) unless it exists, and refuses one that is not a
// real folder of this user that only this user can open: a {tmp} folder someone else made
// could be read or swapped by them.
func ensurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return fmt.Errorf("%s is not a private folder of this user (mode %v)", dir, info.Mode())
	}
	return nil
}

// ensureSandboxTempRoot makes the root of the {tmp} folders before any sandbox starts, so
// every sandbox, including a long-lived one started before the first call, hides it.
func ensureSandboxTempRoot() (string, error) {
	root := sandboxTempRoot()
	if err := ensurePrivateDir(root); err != nil {
		return "", fmt.Errorf("the sandbox temp folder: %w", err)
	}
	return root, nil
}

// callTempDir is the {tmp} folder of one call's children, made when missing: one per session
// (SessionKey), workspace (a run in a worktree of its own gets its own) and form of the
// sandbox (a plan-mode or read_only call does not share one with calls that may write).
// Empty without a sandbox: an unsandboxed child uses the system temp folder as before.
func callTempDir(sandbox *sessionfs.SandboxSettings, execution tools.ExecutionContext) (string, error) {
	if sandbox == nil {
		return "", nil
	}
	root, err := ensureSandboxTempRoot()
	if err != nil {
		return "", err
	}
	form := "write"
	if execution.PlanMode {
		form = "plan"
	}
	digest := sha256.Sum256([]byte(execution.SessionKey + "\x00" + filepath.Clean(execution.WorkspaceRoot) + "\x00" + form))
	dir := filepath.Join(root, hex.EncodeToString(digest[:sandboxTempIDBytes]))
	if err := ensurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("the call's temp folder: %w", err)
	}
	return dir, nil
}

// sandboxWritableRoots are the folders a sandbox binds read-write: the expanded writable
// paths that exist, resolved through symlinks, each once, without those a sandboxed call
// could have renamed or replaced (S5). Such a path is reached through a folder that another
// writable path covers: that path's bind already makes whatever is really there writable,
// and binding the name on its own would follow a symlink the call left in its place.
func sandboxWritableRoots(paths []string, expand func(string) string) []string {
	var resolved []string
	var candidates []string
	for _, path := range paths {
		path = expand(path)
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			// Missing: bwrap's --bind-try left such a path out as well.
			continue
		}
		resolved = append(resolved, real)
		candidates = append(candidates, absolute)
	}
	var roots []string
	for _, candidate := range candidates {
		real, controlled, ok := resolveThrough(candidate, resolved)
		if !ok || controlled || containsString(roots, real) {
			continue
		}
		roots = append(roots, real)
	}
	return roots
}

// sandboxMaxLinks bounds the symlinks resolveThrough follows, as the kernel does.
const sandboxMaxLinks = 40

// resolveThrough resolves path like the kernel, one name at a time, and reports whether it
// looked a name up in a folder inside one of writable: a sandboxed call can rename or replace
// that name, so the path is not one to bind on its own. ok is false when path does not resolve.
func resolveThrough(path string, writable []string) (real string, controlled, ok bool) {
	pending := strings.Split(filepath.Clean(path), string(filepath.Separator))
	current := string(filepath.Separator)
	links := 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		if insideAny(current, writable) {
			controlled = true
		}
		next := filepath.Join(current, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", controlled, false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		if links++; links > sandboxMaxLinks {
			return "", controlled, false
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", controlled, false
		}
		if filepath.IsAbs(target) {
			current = string(filepath.Separator)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return current, controlled, true
}

// insideAny reports whether path is one of roots or lies inside one.
func insideAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathInside(path, root) {
			return true
		}
	}
	return false
}

func pathInside(path, root string) bool {
	if root == string(filepath.Separator) || path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// sandboxRuleKind is what a fixed rule does to its path.
type sandboxRuleKind int

const (
	// sandboxPin binds a writable folder onto itself, so it is a mount point that cannot be
	// renamed, removed or replaced; it stays writable.
	sandboxPin sandboxRuleKind = iota
	// sandboxReadOnly makes a folder or file read-only; a missing folder becomes an empty
	// read-only one, so it cannot be created either.
	sandboxReadOnly
	// sandboxHide replaces a folder with an empty read-only one, then shows keep in it again.
	sandboxHide
)

// sandboxRule is one fixed rule, in the order it applies.
type sandboxRule struct {
	kind sandboxRuleKind
	path string
	// dir says the path is (or for a missing path, would be) a folder.
	dir bool
	// missing says the path does not exist yet.
	missing bool
	// keep are the entries of a hidden folder shown again, each as writable as before.
	keep []sandboxKeep
	// inner are writable folders inside a read-only folder, bound read-write again.
	inner []string
}

type sandboxKeep struct {
	path  string
	write bool
	// inner are writable folders inside a read-only keep, bound read-write again.
	inner []string
}

// sandboxFixedRules are the fixed rules for one call, given the folders the sandbox binds
// read-write (sandboxWritableRoots) and the sandbox, whose ReadOnlyPaths are the session's
// configuration paths. The configuration rules come first: they bind writable folders inside
// them again, which the rules after them then narrow as usual. The sealed folders follow them
// and cover those again (sandboxSealedRules).
func sandboxFixedRules(place sandboxPlace, writable []string, sandbox *sessionfs.SandboxSettings, candidates []string, visible func(string) bool) []sandboxRule {
	rules := sandboxConfigurationRules(place, writable, sandbox.ReadOnlyPaths, candidates)
	rules = append(rules, sandboxSealedRules(writable, sandbox.SealedPaths, sandbox.SealedInner)...)
	rules = append(rules, sandboxProfileRules(sandbox, writable, visible)...)
	if root, err := filepath.EvalSymlinks(sandboxTempRoot()); err == nil {
		var keep []sandboxKeep
		if place.tmp != "" {
			if own, err := filepath.EvalSymlinks(place.tmp); err == nil && pathInside(own, root) && own != root {
				keep = append(keep, sandboxKeepOf(own, writable))
			}
		}
		rules = append(rules, sandboxRule{kind: sandboxHide, path: root, dir: true, keep: keep})
	}
	common := gitCommonDir(place.workspace)
	if common == "" {
		return rules
	}
	common, err := filepath.EvalSymlinks(common)
	if err != nil {
		return rules
	}
	ownGitDir := workspaceGitDir(place.workspace, common)
	if insideAny(common, writable) {
		rules = append(rules, sandboxRule{kind: sandboxPin, path: common, dir: true})
		hooks := filepath.Join(common, "hooks")
		_, statErr := os.Lstat(hooks)
		rules = append(rules, sandboxRule{kind: sandboxReadOnly, path: hooks, dir: true, missing: statErr != nil})
		configs := []string{filepath.Join(common, "config"), filepath.Join(common, "config.worktree")}
		if ownGitDir != common {
			configs = append(configs, filepath.Join(ownGitDir, "config.worktree"))
		}
		for _, config := range configs {
			if info, err := os.Lstat(config); err == nil && info.Mode().IsRegular() {
				rules = append(rules, sandboxRule{kind: sandboxReadOnly, path: config})
			}
		}
	}
	// Other runs' worktree admin folders and checkouts; the call's own stay.
	admin := filepath.Join(common, "worktrees")
	if info, err := os.Lstat(admin); err == nil && info.IsDir() {
		var keep []sandboxKeep
		if ownGitDir != common && pathInside(ownGitDir, admin) && ownGitDir != admin {
			keep = append(keep, sandboxKeepOf(ownGitDir, writable))
		}
		rules = append(rules, sandboxRule{kind: sandboxHide, path: admin, dir: true, keep: keep})
	}
	runs := filepath.Join(common, worktree.DefaultDirName)
	if info, err := os.Lstat(runs); err == nil && info.IsDir() {
		var keep []sandboxKeep
		if workspace, err := filepath.EvalSymlinks(place.workspace); err == nil && pathInside(workspace, runs) && workspace != runs {
			relative, _ := filepath.Rel(runs, workspace)
			first, _, _ := strings.Cut(relative, string(filepath.Separator))
			keep = append(keep, sandboxKeepOf(filepath.Join(runs, first), writable))
		}
		rules = append(rules, sandboxRule{kind: sandboxHide, path: runs, dir: true, keep: keep})
	}
	return append(rules, linkedWorktreeRules(place.workspace, common, ownGitDir, writable)...)
}

// linkedWorktreeRules keep the pointers of a linked worktree read-only where a writable path
// covers them: its .git file and its admin folder's commondir and gitdir. jad finds the
// repository's git directory through them for the next call's sandbox ({git_common_dir}, the
// fixed rules), so a call that rewrote them could point that call's writable paths anywhere.
// They come last, after the hidden worktree folders showed the admin folder again. An
// isolated run's folders are jad's own record (isolatedRunOf) and need none.
func linkedWorktreeRules(workspace, common, ownGitDir string, writable []string) []sandboxRule {
	if ownGitDir == common {
		return nil
	}
	if _, isolated := isolatedRunOf(workspace); isolated {
		return nil
	}
	var rules []sandboxRule
	files := []string{filepath.Join(ownGitDir, "commondir"), filepath.Join(ownGitDir, "gitdir")}
	if dotGit := workspaceDotGitFile(workspace); dotGit != "" {
		files = append(files, dotGit)
	}
	for _, file := range files {
		if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() && insideAny(file, writable) {
			rules = append(rules, sandboxRule{kind: sandboxReadOnly, path: file})
		}
	}
	return rules
}

// workspaceDotGitFile is the .git file of the linked worktree that holds workspace, or empty
// when the nearest .git is a folder or there is none.
func workspaceDotGitFile(workspace string) string {
	for current := filepath.Clean(workspace); ; current = filepath.Dir(current) {
		dotGit := filepath.Join(current, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			if !info.Mode().IsRegular() {
				return ""
			}
			if resolved, err := filepath.EvalSymlinks(dotGit); err == nil {
				return resolved
			}
			return dotGit
		}
		if parent := filepath.Dir(current); parent == current {
			return ""
		}
	}
}

// sandboxConfigurationRules keep the session's configuration read-only where a writable path
// covers it. The session's own state folder comes last, so its transcripts stay read-only even
// when a writable path covers it or it is listed itself ({session_dir}).
//
// A writable path inside such a folder (candidates: the expanded writable paths) is writable
// again when no symlink lies between the folder and it and no other such path holds it: in a
// read-only folder a sandboxed call cannot rename or replace a name, so the S5 rule that left
// the path out of the writable roots does not apply there.
func sandboxConfigurationRules(place sandboxPlace, writable, configuration, candidates []string) []sandboxRule {
	if place.sessionDir != "" {
		configuration = append(append([]string(nil), configuration...), place.sessionDir)
	}
	var rules []sandboxRule
	var done []string
	for _, path := range configuration {
		real, err := filepath.EvalSymlinks(path)
		if err != nil || containsString(done, real) || !insideAny(real, writable) {
			// Missing (nothing to keep) or not writable anyway.
			continue
		}
		info, err := os.Stat(real)
		if err != nil {
			continue
		}
		done = append(done, real)
		rule := sandboxRule{kind: sandboxReadOnly, path: real, dir: info.IsDir()}
		var inside []string
		for _, candidate := range append(append([]string(nil), writable...), candidates...) {
			if candidate != real && pathInside(candidate, real) && !containsString(inside, candidate) && noSymlinkBelow(real, candidate) {
				inside = append(inside, candidate)
			}
		}
		for _, candidate := range inside {
			held := false
			for _, other := range inside {
				if other != candidate && pathInside(candidate, other) {
					held = true
				}
			}
			if !held {
				rule.inner = append(rule.inner, candidate)
			}
		}
		rules = append(rules, rule)
	}
	return rules
}

// noSymlinkBelow reports whether every name from root down to path is a real folder or file.
func noSymlinkBelow(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return false
	}
	current := root
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// sandboxWritableCandidates are the expanded writable paths, absolute and clean, as written:
// sandboxConfigurationRules binds the ones inside a read-only folder again.
func sandboxWritableCandidates(paths []string, expand func(string) string) []string {
	var candidates []string
	for _, path := range paths {
		if path = expand(path); path == "" {
			continue
		}
		if absolute, err := filepath.Abs(path); err == nil {
			candidates = append(candidates, filepath.Clean(absolute))
		}
	}
	return candidates
}

// sandboxReadRoot is one folder or file a sandbox with read paths binds read-only: real is
// where it is, and link, when set, the name the read path gave it, made a symlink to real
// (/lib is a symlink to /usr/lib on most systems).
type sandboxReadRoot struct {
	real string
	link string
}

// sandboxReadRoots are the read paths that exist, {system} expanded, resolved through
// symlinks, each once, without those under a writable path: the writable bind already shows
// what is really there, and resolving a name a sandboxed call could have replaced would show
// whatever its symlink points at (as S5 for writes).
func sandboxReadRoots(paths []string, expand func(string) string, writable []string) []sandboxReadRoot {
	var expanded []string
	for _, path := range paths {
		if path == sessionfs.SandboxSystemPlaceholder {
			expanded = append(expanded, sessionfs.SandboxSystemPaths()...)
			continue
		}
		if path = expand(path); path != "" {
			expanded = append(expanded, path)
		}
	}
	var roots []sandboxReadRoot
	var bound, linked []string
	for _, path := range expanded {
		absolute, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		real, controlled, ok := resolveThrough(absolute, writable)
		if !ok || controlled || insideAny(real, writable) {
			continue
		}
		root := sandboxReadRoot{real: real}
		if absolute != real && !containsString(linked, absolute) {
			// Only a name directly in a folder that exists inside can be a symlink there.
			if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil && parent == filepath.Dir(absolute) {
				root.link = absolute
				linked = append(linked, absolute)
			}
		}
		if containsString(bound, real) && root.link == "" {
			continue
		}
		bound = append(bound, real)
		roots = append(roots, root)
	}
	return roots
}

// sandboxVisible reports whether path is in a sandbox with read paths at all.
func sandboxVisible(path string, readable []sandboxReadRoot, writable []string) bool {
	if insideAny(path, writable) {
		return true
	}
	for _, root := range readable {
		if pathInside(path, root.real) || (root.link != "" && pathInside(path, root.link)) {
			return true
		}
	}
	return false
}

// sandboxKeepOf is path shown again in a hidden folder, writable when a writable root covers
// it, else read-only with the writable roots inside it bound read-write again.
func sandboxKeepOf(path string, writable []string) sandboxKeep {
	keep := sandboxKeep{path: path, write: insideAny(path, writable)}
	if !keep.write {
		for _, root := range writable {
			if pathInside(root, path) {
				keep.inner = append(keep.inner, root)
			}
		}
	}
	return keep
}

// workspaceGitDir is the git folder of the checkout that holds workspace: a linked worktree's
// own admin folder (named by its .git file), else common.
func workspaceGitDir(workspace, common string) string {
	if run, ok := isolatedRunOf(workspace); ok {
		return run.admin
	}
	for current := filepath.Clean(workspace); ; current = filepath.Dir(current) {
		dotGit := filepath.Join(current, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			if info.IsDir() {
				return common
			}
			content, err := os.ReadFile(dotGit)
			if err != nil {
				return common
			}
			gitDir, found := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
			if !found {
				return common
			}
			gitDir = strings.TrimSpace(gitDir)
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(current, gitDir)
			}
			if resolved, err := filepath.EvalSymlinks(gitDir); err == nil {
				return resolved
			}
			return common
		}
		if parent := filepath.Dir(current); parent == current {
			return common
		}
	}
}

// bwrapRuleArguments are the bwrap arguments of the fixed rules.
func bwrapRuleArguments(rules []sandboxRule) []string {
	var arguments []string
	for _, rule := range rules {
		switch rule.kind {
		case sandboxPin:
			arguments = append(arguments, "--bind", rule.path, rule.path)
		case sandboxReadOnly:
			if rule.missing {
				arguments = append(arguments, "--tmpfs", rule.path, "--remount-ro", rule.path)
			} else {
				arguments = append(arguments, "--ro-bind", rule.path, rule.path)
			}
			for _, inner := range rule.inner {
				arguments = append(arguments, "--bind", inner, inner)
			}
		case sandboxHide:
			arguments = append(arguments, "--tmpfs", rule.path)
			for _, keep := range rule.keep {
				if keep.write {
					arguments = append(arguments, "--bind", keep.path, keep.path)
					continue
				}
				arguments = append(arguments, "--ro-bind", keep.path, keep.path)
				for _, inner := range keep.inner {
					arguments = append(arguments, "--bind", inner, inner)
				}
			}
			arguments = append(arguments, "--remount-ro", rule.path)
		}
	}
	return arguments
}

// seatbeltRuleProfile is the seatbelt profile text of the fixed rules; it follows the
// writable paths and comes before the hidden ones, since a later rule wins.
func seatbeltRuleProfile(rules []sandboxRule) string {
	var profile strings.Builder
	for _, rule := range rules {
		path := seatbeltString(rule.path)
		switch rule.kind {
		case sandboxPin:
			profile.WriteString("(deny file-write* (literal " + path + "))\n")
		case sandboxReadOnly:
			if rule.dir {
				profile.WriteString("(deny file-write* (literal " + path + ") (subpath " + path + "))\n")
			} else {
				profile.WriteString("(deny file-write* (literal " + path + "))\n")
			}
			for _, inner := range rule.inner {
				profile.WriteString("(allow file-write* (subpath " + seatbeltString(inner) + "))\n")
			}
		case sandboxHide:
			profile.WriteString("(deny file-read* file-write* (subpath " + path + "))\n(allow file-read-metadata (literal " + path + "))\n")
			for _, keep := range rule.keep {
				kept := seatbeltString(keep.path)
				profile.WriteString("(allow file-read* (subpath " + kept + "))\n")
				if keep.write {
					profile.WriteString("(allow file-write* (subpath " + kept + "))\n")
				}
				for _, inner := range keep.inner {
					profile.WriteString("(allow file-write* (subpath " + seatbeltString(inner) + "))\n")
				}
			}
		}
	}
	return profile.String()
}

// sandboxSealedRules keep each sealed folder (SandboxSettings.SealedPaths: G and W/.jad in the
// workspace layout) read-only as a whole where a writable path covers it. Unlike a
// configuration path, no writable path is bound again inside it, only the engine's helper
// folders of inner that a writable path covers too (made when missing, so a covered tool can
// keep its helpers); a missing sealed folder whose parent exists becomes an empty read-only
// folder, so nothing can be made there. A sealed folder whose parent is missing too is left
// out (docs/design_workspace_layout.md §6).
func sandboxSealedRules(writable, sealed, inner []string) []sandboxRule {
	var rules []sandboxRule
	var done []string
	for _, path := range sealed {
		real, err := filepath.EvalSymlinks(path)
		missing := false
		if err != nil {
			parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
			if !errors.Is(err, os.ErrNotExist) || parentErr != nil {
				continue
			}
			real, missing = filepath.Join(parent, filepath.Base(path)), true
		}
		if containsString(done, real) || !insideAny(real, writable) {
			continue
		}
		done = append(done, real)
		rule := sandboxRule{kind: sandboxReadOnly, path: real, dir: true, missing: missing}
		if !missing {
			if info, err := os.Stat(real); err != nil {
				continue
			} else if !info.IsDir() {
				rule.dir = false
			}
		}
		if rule.dir && !missing {
			for _, helper := range inner {
				parent, err := filepath.EvalSymlinks(filepath.Dir(helper))
				if err != nil {
					continue
				}
				helper = filepath.Join(parent, filepath.Base(helper))
				if !pathInside(helper, real) || helper == real || !insideAny(helper, writable) || containsString(rule.inner, helper) {
					continue
				}
				if info, err := os.Lstat(helper); err != nil {
					if !errors.Is(err, os.ErrNotExist) || os.Mkdir(helper, 0o755) != nil {
						continue
					}
				} else if !info.IsDir() {
					// A link or a file in its place: nothing is bound writable.
					continue
				}
				rule.inner = append(rule.inner, helper)
			}
		}
		rules = append(rules, rule)
	}
	return rules
}
