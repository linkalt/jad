package sessionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SandboxWidensPrefix starts the error of a sandbox that would reach more than the one it runs
// under, so a caller can tell it from a broken setting.
const SandboxWidensPrefix = "widens: "

// SandboxExpander fills the placeholders of a sandbox path for one run; "" drops the path.
// NarrowSandbox compares paths symbolically when it has none.
type SandboxExpander func(string) string

// NarrowSandbox is the sandbox of a run started by a run with sandbox parent (a sub-agent of
// a session, or the target of a profile_message), given the child's own: a child is never
// wider than the run that starts it, and one that would be is refused (an error starting
// with SandboxWidensPrefix), never silently cut (docs/isolation-design §14.5, D18).
//
//   - no sandbox: a nil parent gives the child's own; a nil child inherits the parent's.
//   - backend: equal; a command backend's command identical.
//   - tools, hide_paths, read-only paths: both together.
//   - read paths: each of the child's inside one of the parent's read or write paths.
//   - write paths and plan-mode write paths: each inside one of the parent's of the same kind.
//   - network: the child's only when the parent has it; network_allow hosts inside the
//     parent's (a host or a subdomain of one).
//   - isolation: the child's.
//
// With nil expanders paths compare by placeholder and remainder ({workspace}/src lies inside
// {workspace}), which is right for runs of one session, where each run fills them in itself.
// A target in another session compares expanded paths (parentExpand for the parent's,
// childExpand for the child's); {tmp} and {system} still compare by name, since {tmp} is
// each run's own scratch folder.
func NarrowSandbox(parent, child *SandboxSettings, parentExpand, childExpand SandboxExpander) (*SandboxSettings, error) {
	if parent == nil {
		return child, nil
	}
	if child == nil {
		return parent, nil
	}
	if parent.Backend != child.Backend {
		return nil, fmt.Errorf("%sbackend %q is not the parent's %q", SandboxWidensPrefix, child.Backend, parent.Backend)
	}
	if parent.Backend == SandboxBackendCommand && !slices.Equal(parent.Command, child.Command) {
		return nil, fmt.Errorf("%scommand %q is not the parent's %q", SandboxWidensPrefix, child.Command, parent.Command)
	}
	readable := append(append([]string(nil), parent.ReadPaths...), parent.WritablePaths...)
	checks := []struct {
		name          string
		child, parent []string
	}{
		{"read.paths", child.ReadPaths, readable},
		{"write.paths", child.WritablePaths, parent.WritablePaths},
		{"write.plan_paths", child.PlanWritablePaths, parent.PlanWritablePaths},
	}
	for _, check := range checks {
		for _, entry := range check.child {
			if !sandboxEntryCovered(entry, check.parent, childExpand, parentExpand) {
				return nil, fmt.Errorf("%s%s entry %q is not inside the parent's (%s)", SandboxWidensPrefix, check.name, entry, strings.Join(check.parent, ", "))
			}
		}
	}
	if child.Network && !parent.Network {
		return nil, fmt.Errorf("%snetwork is on, and the parent has no network", SandboxWidensPrefix)
	}
	if !parent.Network {
		if len(child.NetworkAllow) > 0 && len(parent.NetworkAllow) == 0 {
			return nil, fmt.Errorf("%snetwork_allow %s, and the parent has no network", SandboxWidensPrefix, strings.Join(child.NetworkAllow, ", "))
		}
		for _, host := range child.NetworkAllow {
			if !hostCovered(host, parent.NetworkAllow) {
				return nil, fmt.Errorf("%snetwork_allow host %q is not one of the parent's (%s) or a subdomain of one", SandboxWidensPrefix, host, strings.Join(parent.NetworkAllow, ", "))
			}
		}
	}
	narrowed := *child
	narrowed.Tools = appendNewStrings(append([]string(nil), parent.Tools...), child.Tools...)
	narrowed.HidePaths = appendNewStrings(append([]string(nil), child.HidePaths...), expandedPaths(parent.HidePaths, parentExpand)...)
	narrowed.ReadOnlyPaths = appendNewStrings(append([]string(nil), child.ReadOnlyPaths...), parent.ReadOnlyPaths...)
	if len(child.NetworkAllow) == 0 && !child.Network && len(parent.NetworkAllow) > 0 {
		// A child that names no host gets none: no network is narrower than the parent's.
		narrowed.NetworkAllow = nil
	}
	return &narrowed, nil
}

// InheritSandbox is parent's sandbox for a run in another session that names none of its own
// (a profile_message target): its paths filled in for the parent's run, so {workspace} still
// means the caller's folder and not the target's. {tmp} and {system} stay placeholders.
func InheritSandbox(parent *SandboxSettings, parentExpand SandboxExpander) *SandboxSettings {
	if parent == nil || parentExpand == nil {
		return parent
	}
	inherited := *parent
	inherited.ReadPaths = expandedPaths(parent.ReadPaths, parentExpand)
	inherited.WritablePaths = expandedPaths(parent.WritablePaths, parentExpand)
	inherited.PlanWritablePaths = expandedPaths(parent.PlanWritablePaths, parentExpand)
	inherited.HidePaths = expandedPaths(parent.HidePaths, parentExpand)
	return &inherited
}

// expandedPaths are paths filled in by expand (none when expand is nil; every {tmp} and
// {system} entry is kept as it is, for the run that uses it to fill in), without the ones
// that name nothing.
func expandedPaths(paths []string, expand SandboxExpander) []string {
	if expand == nil {
		return append([]string(nil), paths...)
	}
	var result []string
	for _, path := range paths {
		if keepsPlaceholder(path) {
			result = appendNewStrings(result, path)
			continue
		}
		if filled := expand(path); filled != "" {
			result = appendNewStrings(result, filepath.Clean(filled))
		}
	}
	return result
}

// keepsPlaceholder says a path is compared and kept by name across sessions.
func keepsPlaceholder(path string) bool {
	return path == SandboxSystemPlaceholder || strings.HasPrefix(path, "{tmp}")
}

// sandboxEntryCovered reports whether entry (of the child) lies inside one of parents.
func sandboxEntryCovered(entry string, parents []string, childExpand, parentExpand SandboxExpander) bool {
	if entry == SandboxSystemPlaceholder {
		if containsString(parents, SandboxSystemPlaceholder) || containsString(parents, string(filepath.Separator)) {
			return true
		}
		for _, system := range SandboxSystemPaths() {
			if _, err := os.Lstat(system); err == nil && !sandboxEntryCovered(system, parents, nil, parentExpand) {
				return false
			}
		}
		return true
	}
	cross := childExpand != nil || parentExpand != nil
	if cross && !keepsPlaceholder(entry) {
		if childExpand != nil {
			if entry = childExpand(entry); entry == "" {
				// Names nothing for the child, so it adds nothing to its environment.
				return true
			}
		}
	}
	for _, parent := range parents {
		if parent == string(filepath.Separator) {
			return true
		}
		if parent == SandboxSystemPlaceholder {
			for _, system := range SandboxSystemPaths() {
				if symbolicPathWithin(entry, system) {
					return true
				}
			}
			continue
		}
		if cross && !keepsPlaceholder(parent) && parentExpand != nil {
			if parent = parentExpand(parent); parent == "" {
				continue
			}
		}
		if symbolicPathWithin(entry, parent) {
			return true
		}
	}
	return false
}

// symbolicPathWithin reports whether path lies inside root, both either absolute or starting
// with a placeholder: a placeholder only contains paths that start with the same one.
func symbolicPathWithin(path, root string) bool {
	pathHead, pathRest := splitPlaceholder(path)
	rootHead, rootRest := splitPlaceholder(root)
	if pathHead != rootHead {
		return false
	}
	return PathWithin(rootRest, pathRest)
}

// splitPlaceholder splits "{name}/rest" into "{name}" and "/rest" (a lone "{name}" into
// "{name}" and "/"); an absolute path has no head.
func splitPlaceholder(path string) (string, string) {
	if !strings.HasPrefix(path, "{") {
		return "", filepath.Clean(path)
	}
	end := strings.Index(path, "}")
	if end < 0 {
		return path, string(filepath.Separator)
	}
	rest := path[end+1:]
	if rest == "" {
		rest = string(filepath.Separator)
	}
	return path[:end+1], filepath.Clean(string(filepath.Separator) + strings.TrimPrefix(rest, string(filepath.Separator)))
}

// hostCovered reports whether host is one of hosts or a subdomain of one.
func hostCovered(host string, hosts []string) bool {
	for _, allowed := range hosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// GitCommonDir is the git directory of the repository that holds dir: the .git folder of a
// checkout, or for a worktree (whose .git is a file naming its own git folder) the folder its
// commondir names. Empty when dir is in no repository.
func GitCommonDir(dir string) string {
	if dir == "" {
		return ""
	}
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		dotGit := filepath.Join(current, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return dotGit
			}
			return worktreeCommonDir(current, dotGit)
		}
		if parent := filepath.Dir(current); parent == current {
			return ""
		}
	}
}

func worktreeCommonDir(top, dotGit string) string {
	content, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	gitDir, found := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
	if !found {
		return ""
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	return filepath.Clean(commonDir)
}

// SandboxPlace is what the placeholders of one run stand for, outside a call: its workspace
// and working directory, its session's state folder. {tmp} has no value here (it is each
// call's own); {home} is the user's home folder.
type SandboxPlace struct {
	Workspace  string
	CWD        string
	SessionDir string
}

// Expander fills the placeholders of place; a path whose placeholder names nothing is "".
// {tmp} and {system} are left as they are.
func (place SandboxPlace) Expander() SandboxExpander {
	home, _ := os.UserHomeDir()
	values := map[string]string{"{workspace}": place.Workspace, "{cwd}": place.CWD, "{home}": home, "{session_dir}": place.SessionDir}
	gitDirKnown := false
	return func(value string) string {
		if keepsPlaceholder(value) {
			return value
		}
		if strings.Contains(value, "{git_common_dir}") && !gitDirKnown {
			values["{git_common_dir}"], gitDirKnown = GitCommonDir(place.Workspace), true
		}
		pairs := make([]string, 0, 2*len(values))
		for name, filled := range values {
			if strings.Contains(value, name) && filled == "" {
				return ""
			}
			pairs = append(pairs, name, filled)
		}
		return strings.NewReplacer(pairs...).Replace(value)
	}
}
