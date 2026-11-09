package sessionfs

import "encoding/json"

// removedApprovalHint is appended to every removed-key error: a silently ignored
// approval setting would leave the user believing a protection is on.
const removedApprovalHint = "tools now run without asking, so run JAD inside a container that holds only what the agent may touch; delete the key"

// rejectRemovedApproval refuses an `approval` key at fieldPath, naming it. The approval
// system was removed and its keys are refused permanently, never ignored.
func rejectRemovedApproval(fields map[string]json.RawMessage, fieldPath, settingPath string) error {
	if _, exists := fields["approval"]; exists {
		return configError("%s: %s.approval was removed; %s", settingPath, fieldPath, removedApprovalHint)
	}
	return nil
}

// removedAgentPathsHint: an agent's paths only narrowed its write and edit tools, which
// exec replaced, and paths was always refused next to a shell.
const removedAgentPathsHint = "it narrowed the agent's write and edit, which were removed; exec cannot be confined by paths, so run the agent in a worktree (isolation) or a container instead; delete the key"

// removedProcessMaxRunningHint: the per-child process limit went with the process tool.
const removedProcessMaxRunningHint = "it limited a child's managed processes, and tools.native.process was removed; delete the key"

// removedAllowWriteHint: allow_write only existed to gate children behind approvals.
const removedAllowWriteHint = "an agent's tools list alone decides whether a child can write, edit or run commands; delete the key"

// removedAgentReadOnlyHint: an agent's read_only made its exec calls read-only. Subagents have no
// read-only mode now; a child's protection is its tool set and isolation, and ignoring the key
// would turn an agent that was protected into one that writes the workspace.
const removedAgentReadOnlyHint = "protection now comes only from isolation; remove the key, and set isolation to \"worktree\" if this child must not write the workspace"
