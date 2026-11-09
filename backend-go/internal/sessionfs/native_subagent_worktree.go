package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Isolation values of an agent: "none" runs the child in the session's working directory,
// "worktree" in a git worktree of its own whose changes come back as a branch.
const (
	SubagentIsolationNone     = "none"
	SubagentIsolationWorktree = "worktree"
)

const (
	defaultNativeSubagentWorktreeGit          = "git"
	defaultNativeSubagentWorktreeBranchPrefix = "jad/"
	defaultNativeSubagentWorktreeAuthorName   = "jad"
	defaultNativeSubagentWorktreeAuthorEmail  = "jad@localhost"
	defaultNativeSubagentWorktreeTimeoutMS    = int64(60_000)
	maxNativeSubagentWorktreeTimeoutMS        = int64(600_000)
)

var nativeSubagentWorktreeKeys = []string{"dir", "branch_prefix", "git", "author_name", "author_email", "commit_hooks", "timeout_ms"}

// NativeSubagentWorktreeSettings configure the git worktrees of the agents with isolation
// worktree.
type NativeSubagentWorktreeSettings struct {
	// Dir holds one worktree folder per run, absolute; empty means a folder inside the
	// repository's git directory, where the session's search tools never look.
	Dir string
	// BranchPrefix starts each run's branch name; the run id follows it.
	BranchPrefix string
	// Git is the git executable.
	Git string
	// AuthorName and AuthorEmail sign the commit of a run's changes when the repository
	// has no identity of its own.
	AuthorName  string
	AuthorEmail string
	// CommitHooks runs the repository's commit hooks for that commit.
	CommitHooks bool
	// TimeoutMS bounds each git command.
	TimeoutMS int64
}

// DefaultNativeSubagentWorktreeSettings are the settings of a section without worktree.
func DefaultNativeSubagentWorktreeSettings() NativeSubagentWorktreeSettings {
	return NativeSubagentWorktreeSettings{
		BranchPrefix: defaultNativeSubagentWorktreeBranchPrefix,
		Git:          defaultNativeSubagentWorktreeGit,
		AuthorName:   defaultNativeSubagentWorktreeAuthorName,
		AuthorEmail:  defaultNativeSubagentWorktreeAuthorEmail,
		TimeoutMS:    defaultNativeSubagentWorktreeTimeoutMS,
	}
}

// Isolated reports whether the agent's runs work in a git worktree of their own.
func (a NativeSubagentAgent) Isolated() bool { return a.Isolation == SubagentIsolationWorktree }

// ChangesWorkspace reports whether a run of the agent can change the session's own files:
// it can change files and does not work in a worktree of its own.
func (a NativeSubagentAgent) ChangesWorkspace() bool { return a.ChangesFiles() && !a.Isolated() }

// subagentIsolation reads an agent's isolation key; absent or null is none. An agent that
// cannot change files has nothing to isolate, so worktree is refused for it.
func subagentIsolation(fields map[string]json.RawMessage, agent NativeSubagentAgent, settingPath, section string) (string, error) {
	value, err := optionalSubagentString(fields, "isolation", settingPath, section)
	if err != nil {
		return "", err
	}
	switch value {
	case "", SubagentIsolationNone:
		return "", nil
	case SubagentIsolationWorktree:
		if !agent.ChangesFiles() {
			return "", configError("%s: %s.isolation worktree needs a tool that changes files (%s) in tools, or no tools list", settingPath, section, strings.Join(agentChangeTools, ", "))
		}
		return value, nil
	}
	return "", configError("%s: %s.isolation must be %q or %q", settingPath, section, SubagentIsolationNone, SubagentIsolationWorktree)
}

// normalizeNativeSubagentWorktree reads tools.native.subagent.worktree. A relative dir is
// taken from the session's working directory.
func normalizeNativeSubagentWorktree(raw json.RawMessage, cwd, settingPath string) (NativeSubagentWorktreeSettings, error) {
	settings := DefaultNativeSubagentWorktreeSettings()
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return settings, nil
	}
	const section = "tools.native.subagent.worktree"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return NativeSubagentWorktreeSettings{}, configError("%s: %s must be an object", settingPath, section)
	}
	for key := range fields {
		if !containsString(nativeSubagentWorktreeKeys, key) {
			return NativeSubagentWorktreeSettings{}, unsupportedToolKey(settingPath+": "+section, key, nativeSubagentWorktreeKeys, "")
		}
	}
	dir, err := optionalSubagentString(fields, "dir", settingPath, section)
	if err != nil {
		return NativeSubagentWorktreeSettings{}, err
	}
	if dir != "" {
		dir = expandHome(dir)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		settings.Dir = filepath.Clean(dir)
	}
	for _, text := range []struct {
		name   string
		target *string
	}{{"branch_prefix", &settings.BranchPrefix}, {"git", &settings.Git}, {"author_name", &settings.AuthorName}, {"author_email", &settings.AuthorEmail}} {
		value, err := optionalSubagentString(fields, text.name, settingPath, section)
		if err != nil {
			return NativeSubagentWorktreeSettings{}, err
		}
		if value != "" {
			*text.target = value
		}
	}
	if strings.ContainsAny(settings.BranchPrefix, " ~^:?*[\\") || strings.Contains(settings.BranchPrefix, "..") || strings.HasPrefix(settings.BranchPrefix, "-") || strings.HasPrefix(settings.BranchPrefix, "/") {
		return NativeSubagentWorktreeSettings{}, configError("%s: %s.branch_prefix %q cannot start a git branch name", settingPath, section, settings.BranchPrefix)
	}
	if settings.CommitHooks, err = bashBool(fields, "commit_hooks", false, settingPath+": "+section); err != nil {
		return NativeSubagentWorktreeSettings{}, err
	}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultNativeSubagentWorktreeTimeoutMS, 1000, maxNativeSubagentWorktreeTimeoutMS, settingPath, section); err != nil {
		return NativeSubagentWorktreeSettings{}, err
	}
	return settings, nil
}
