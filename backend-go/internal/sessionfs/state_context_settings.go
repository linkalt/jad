package sessionfs

import (
	"encoding/json"
	"strings"
)

// StateContextSettings controls optional context snapshots derived after
// compaction. Configured native task, process, memory, subagent and request_draft
// providers are supported.
type StateContextSettings struct {
	Enabled  bool
	Trigger  []string
	Tools    []string
	Role     string
	MaxBytes int64
}

func normalizeStateContextSettings(
	setting map[string]json.RawMessage,
	settingPath string,
	process *NativeProcessSettings,
	task *NativeTaskSettings,
	memory *NativeMemorySettings,
	subagent *NativeSubagentSettings,
	requestDraft *NativeRequestDraftSettings,
) (StateContextSettings, error) {
	settings := StateContextSettings{
		Trigger:  []string{"compaction"},
		Tools:    []string{},
		Role:     "developer",
		MaxBytes: defaultStateContextMaxBytes,
	}
	raw, exists := setting["statecontext"]
	if !exists {
		return settings, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return StateContextSettings{}, configError("%s: statecontext must be an object", settingPath)
	}
	fields, err := decodeConfigObject(raw, settingPath+": statecontext")
	if err != nil {
		return StateContextSettings{}, err
	}
	if value, exists := fields["enabled"]; exists && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.Enabled); err != nil {
			return StateContextSettings{}, configError("%s: statecontext.enabled must be a boolean", settingPath)
		}
	}
	if value, exists := fields["trigger"]; exists && strings.TrimSpace(string(value)) != "null" {
		var rawTriggers []json.RawMessage
		if err := json.Unmarshal(value, &rawTriggers); err != nil || rawTriggers == nil {
			return StateContextSettings{}, configError("%s: statecontext.trigger currently supports only compaction", settingPath)
		}
		settings.Trigger = make([]string, 0, len(rawTriggers))
		seen := make(map[string]struct{}, len(rawTriggers))
		for _, rawTrigger := range rawTriggers {
			var trigger string
			if err := json.Unmarshal(rawTrigger, &trigger); err != nil || trigger != "compaction" {
				return StateContextSettings{}, configError("%s: statecontext.trigger currently supports only compaction", settingPath)
			}
			if _, exists := seen[trigger]; !exists {
				settings.Trigger = append(settings.Trigger, trigger)
				seen[trigger] = struct{}{}
			}
		}
	}
	if value, exists := fields["tools"]; exists && strings.TrimSpace(string(value)) != "null" {
		var rawTools []json.RawMessage
		if err := json.Unmarshal(value, &rawTools); err != nil || rawTools == nil {
			return StateContextSettings{}, configError("%s: statecontext.tools must contain only task, memory, process, subagent, or request_draft", settingPath)
		}
		settings.Tools = make([]string, 0, len(rawTools))
		seen := make(map[string]struct{}, len(rawTools))
		for _, rawTool := range rawTools {
			var name string
			if err := json.Unmarshal(rawTool, &name); err != nil || (name != "task" && name != "memory" && name != "process" && name != "subagent" && name != "request_draft") {
				return StateContextSettings{}, configError("%s: statecontext.tools must contain only task, memory, process, subagent, or request_draft", settingPath)
			}
			if _, exists := seen[name]; !exists {
				settings.Tools = append(settings.Tools, name)
				seen[name] = struct{}{}
			}
		}
	}
	if value, exists := fields["role"]; exists && strings.TrimSpace(string(value)) != "null" {
		if err := json.Unmarshal(value, &settings.Role); err != nil || (settings.Role != "system" && settings.Role != "developer") {
			return StateContextSettings{}, configError("%s: statecontext.role must be system or developer", settingPath)
		}
	}
	if settings.MaxBytes, err = configuredInteger(fields, "max_bytes", defaultStateContextMaxBytes, 1024, maxStateContextBytes, settingPath, "statecontext"); err != nil {
		return StateContextSettings{}, err
	}
	if settings.Enabled {
		if len(settings.Trigger) == 0 {
			return StateContextSettings{}, configError("%s: statecontext.trigger must not be empty when statecontext is enabled", settingPath)
		}
		if len(settings.Tools) == 0 {
			return StateContextSettings{}, configError("%s: statecontext.tools must not be empty when statecontext is enabled", settingPath)
		}
		for _, name := range settings.Tools {
			switch name {
			case "process":
				if process == nil {
					return StateContextSettings{}, configError("%s: statecontext provider process requires tools.native.process", settingPath)
				}
			case "task":
				if task == nil {
					return StateContextSettings{}, configError("%s: statecontext provider task requires tools.native.task", settingPath)
				}
			case "request_draft":
				if requestDraft == nil {
					return StateContextSettings{}, configError("%s: statecontext provider request_draft requires tools.native.request_draft", settingPath)
				}
			case "subagent":
				if subagent == nil {
					return StateContextSettings{}, configError("%s: statecontext provider subagent requires tools.native.subagent", settingPath)
				}
			case "memory":
				if memory == nil {
					return StateContextSettings{}, configError("%s: statecontext provider memory requires tools.native.memory", settingPath)
				}
				if len(memory.StateContext) == 0 {
					return StateContextSettings{}, configError("%s: statecontext provider memory requires at least one tools.native.memory.statecontext selector", settingPath)
				}
			}
		}
	}
	return settings, nil
}
