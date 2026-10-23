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

// removedAllowWriteHint: allow_write only existed to gate children behind approvals.
const removedAllowWriteHint = "an agent's tools list alone decides whether a child can write, edit or run commands; delete the key"
