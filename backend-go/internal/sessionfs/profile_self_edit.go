package sessionfs

import (
	"encoding/json"
	"strings"
)

// The levels of profile_self_edit (docs/design_workspace_layout.md §4.7, decider S1): off
// refuses the profile tool's edit and revert, on lets a profile change what it runs with but
// not its ceilings, commands or paths, full lets it change those too. Every level keeps the
// checks a load makes, so no level lets a profile pass a ceiling the defaults or the session set.
const (
	ProfileSelfEditOff  = "off"
	ProfileSelfEditOn   = "on"
	ProfileSelfEditFull = "full"
)

// profileSelfEditKey is the top-level key of G/global.json and W/.jad/workspace.json. A profile
// or toolset file cannot carry it: their keys are fixed (readLayoutFile refuses unknown ones).
const profileSelfEditKey = "profile_self_edit"

// selfEditRank orders the levels; a workspace may lower the global level, never raise it.
func selfEditRank(level string) int {
	switch level {
	case ProfileSelfEditOff:
		return 0
	case ProfileSelfEditOn:
		return 1
	case ProfileSelfEditFull:
		return 2
	}
	return -1
}

// readProfileSelfEdit reads profile_self_edit of a global.json or workspace.json; "" when the
// file does not set it.
func readProfileSelfEdit(setting map[string]json.RawMessage, where string) (string, error) {
	raw, exists := setting[profileSelfEditKey]
	if !exists {
		return "", nil
	}
	var level string
	if err := json.Unmarshal(raw, &level); err != nil || selfEditRank(strings.TrimSpace(level)) < 0 {
		return "", configError("%s: %s must be \"off\", \"on\" or \"full\"", where, profileSelfEditKey)
	}
	return strings.TrimSpace(level), nil
}

// effectiveSelfEdit is the lower of the global level (on when unset) and the workspace's (no
// limit when unset): a cloned repository's workspace.json cannot turn full on (decider S1).
func effectiveSelfEdit(global, workspace string) string {
	if global == "" {
		global = ProfileSelfEditOn
	}
	if workspace != "" && selfEditRank(workspace) < selfEditRank(global) {
		return workspace
	}
	return global
}
