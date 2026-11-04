package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	// DefaultPlanModeInstructions is the message a plan-mode run sends after the history.
	DefaultPlanModeInstructions = "Plan mode is on for this turn. Study the task with the tools you have, which only read, then answer with a plan: the goal, what you would change and where, the steps in order, the risks, and how you would check the result. Do not start the work: the tools that change files or run commands are not available in this turn. The person reads the plan and starts the work with their next message."
	defaultPlanModeRole         = "developer"
	maxPlanModeInstructionBytes = 64 * 1024
)

var planModeKeys = []string{"allow_tools", "instructions", "role"}

// PlanModeSettings shape a run started in plan mode: it sees only the tools that never
// change anything, plus AllowTools, and Instructions is sent after the history in every
// request of the run, never stored in the transcript.
type PlanModeSettings struct {
	// Instructions is the text of the plan-mode message.
	Instructions string
	// Role is the role of that message: "developer" or "system".
	Role string
	// AllowTools are tools.allow entries kept in plan mode although they can change
	// something (a native tool name, mcp:<server>:<tool> or mcp:<server>:*). tools.deny
	// and tools.allow still apply to them.
	AllowTools []string
}

// DefaultPlanModeSettings are the settings of a setting file without plan_mode.
func DefaultPlanModeSettings() PlanModeSettings {
	return PlanModeSettings{Instructions: DefaultPlanModeInstructions, Role: defaultPlanModeRole, AllowTools: []string{}}
}

// normalizePlanModeSettings reads the `plan_mode` node. Only an absent key takes its
// default.
func normalizePlanModeSettings(raw json.RawMessage, settingPath string) (PlanModeSettings, error) {
	settings := DefaultPlanModeSettings()
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": plan_mode")
	if err != nil {
		return PlanModeSettings{}, configError("%s: plan_mode must be an object", settingPath)
	}
	for key := range fields {
		if !containsString(planModeKeys, key) {
			return PlanModeSettings{}, unsupportedToolKey(settingPath+": plan_mode", key, planModeKeys, "")
		}
	}
	if value, exists := fields["instructions"]; exists {
		var text string
		if json.Unmarshal(value, &text) != nil || strings.TrimSpace(text) == "" || len(text) > maxPlanModeInstructionBytes {
			return PlanModeSettings{}, configError("%s: plan_mode.instructions must be a non-empty string of at most %d bytes", settingPath, maxPlanModeInstructionBytes)
		}
		settings.Instructions = text
	}
	if value, exists := fields["role"]; exists {
		var role string
		if json.Unmarshal(value, &role) != nil || (role != "developer" && role != "system") {
			return PlanModeSettings{}, configError("%s: plan_mode.role must be \"developer\" or \"system\"", settingPath)
		}
		settings.Role = role
	}
	if value, exists := fields["allow_tools"]; exists {
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil || items == nil {
			return PlanModeSettings{}, configError("%s: plan_mode.allow_tools must be an array of tool names", settingPath)
		}
		for index, item := range items {
			var entry string
			if err := json.Unmarshal(item, &entry); err != nil || entry == "" {
				return PlanModeSettings{}, configError("%s: plan_mode.allow_tools[%d] must be a non-empty string", settingPath, index)
			}
			if err := checkProfileToolEntry(entry); err != nil {
				return PlanModeSettings{}, configError("%s: plan_mode.allow_tools[%d] %v", settingPath, index, err)
			}
			if containsString(settings.AllowTools, entry) {
				return PlanModeSettings{}, configError("%s: plan_mode.allow_tools[%d] repeats %q", settingPath, index, entry)
			}
			settings.AllowTools = append(settings.AllowTools, entry)
		}
	}
	return settings, nil
}
