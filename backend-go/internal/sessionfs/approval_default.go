package sessionfs

import "encoding/json"

// normalizeApprovalDefault reads approval.default: the approval of every tool that can
// ask whose own key is absent. Only an absent key means off; null is an error, like the
// per-tool keys.
func normalizeApprovalDefault(fields map[string]json.RawMessage, settingPath string) (string, error) {
	raw, exists := fields["default"]
	if !exists {
		return ApprovalOff, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil || (mode != ApprovalOff && mode != ApprovalAsk) {
		return "", configError("%s: approval.default must be \"off\" or \"ask\"", settingPath)
	}
	return mode, nil
}

// nativeToolHasApprovalKey reports whether tools.native.<name> sets its own `approval`.
// The per-tool normalizers have already validated the document, so a shape that does not
// decode counts as "set": the default never overrides something this cannot read.
func nativeToolHasApprovalKey(setting map[string]json.RawMessage, name string) bool {
	var tools map[string]json.RawMessage
	if json.Unmarshal(setting["tools"], &tools) != nil {
		return true
	}
	var native map[string]json.RawMessage
	if json.Unmarshal(tools["native"], &native) != nil {
		return true
	}
	var tool map[string]json.RawMessage
	if json.Unmarshal(native[name], &tool) != nil {
		return true
	}
	_, exists := tool["approval"]
	return exists
}

// applyApprovalDefault gives every tool that can ask and has no `approval` key of its own
// the session's approval.default. An explicit per-tool value, off or ask, always wins, and
// a tool the session does not configure stays absent. Tools that cannot ask are untouched.
func applyApprovalDefault(
	mode string,
	setting map[string]json.RawMessage,
	write *NativeWriteSettings,
	edit *NativeEditSettings,
	bash *NativeBashSettings,
	exec *NativeExecSettings,
	process *NativeProcessSettings,
	servers []MCPServerReference,
) {
	if mode != ApprovalAsk {
		return
	}
	if write != nil && !nativeToolHasApprovalKey(setting, "write") {
		write.Approval = mode
	}
	if edit != nil && !nativeToolHasApprovalKey(setting, "edit") {
		edit.Approval = mode
	}
	if bash != nil && !nativeToolHasApprovalKey(setting, "bash") {
		bash.Approval = mode
	}
	if exec != nil && !nativeToolHasApprovalKey(setting, "exec") {
		exec.Approval = mode
	}
	if process != nil && !nativeToolHasApprovalKey(setting, "process") {
		process.Approval = mode
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(setting["mcp_servers"], &entries) != nil {
		return
	}
	for index := range servers {
		if index >= len(entries) {
			break
		}
		if _, exists := entries[index]["approval"]; !exists {
			servers[index].Approval = mode
		}
	}
}
