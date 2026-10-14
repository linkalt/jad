package sessionfs

import (
	"encoding/json"
	"strings"
)

// sessionPathFields mirrors SESSION_PATH_FIELDS plus tool_storage in the
// TypeScript configuration loader: the only storage keys a path profile may set
// besides its model and prompt settings.
var sessionPathFields = map[string]struct{}{
	"messages": {}, "messages_backup": {}, "compaction_cache": {},
	"usage_cache": {}, "cursor_cache": {}, "run_error_cache": {}, "statecontext": {}, "staging": {},
	"http_debug": {}, "tool_storage": {},
}

const removedProcessTaskPathsMessage = "%s: paths.processes and paths.tasks were removed; use tools.native.process.storage_dir and tools.native.task.storage_dir"

// validateProfileFields applies the TypeScript per-profile shape checks to one
// paths.profiles entry. Fields are visited in document order, as TypeScript does.
func validateProfileFields(name string, raw json.RawMessage, settingPath string) error {
	profilePath := settingPath + ": paths.profiles." + name
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return configError("%s must be an object", profilePath)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return configError("%s must be an object", profilePath)
	}
	var modelDefault string
	var models []string
	hasModelDefault, hasModels := false, false
	for _, field := range keys {
		value := fields[field]
		switch field {
		case "processes", "tasks":
			return configError(removedProcessTaskPathsMessage, settingPath)
		case "model":
			return configError("%s.model was removed; use model_default and models for model config paths", profilePath)
		case "model_default":
			text, ok := nonEmptyJSONString(value)
			if !ok {
				return configError("%s: model_default must be a non-empty string", profilePath)
			}
			modelDefault, hasModelDefault = text, true
		case "models":
			var items []json.RawMessage
			if err := json.Unmarshal(value, &items); err != nil || items == nil {
				return configError("%s: models must be an array of model configuration paths", profilePath)
			}
			models = make([]string, 0, len(items))
			seen := make(map[string]struct{}, len(items))
			for index, item := range items {
				text, ok := nonEmptyJSONString(item)
				if !ok {
					return configError("%s: models[%d] must be a non-empty string", profilePath, index)
				}
				models = append(models, text)
				seen[text] = struct{}{}
			}
			if len(seen) != len(models) {
				return configError("%s: models must not contain duplicate paths", profilePath)
			}
			hasModels = true
		case "prompt_documents":
			var items []json.RawMessage
			if err := json.Unmarshal(value, &items); err != nil || items == nil {
				return configError("%s: prompt_documents must be an array", profilePath)
			}
			for index, item := range items {
				if _, ok := nonEmptyJSONString(item); !ok {
					return configError("%s: prompt_documents[%d] must be a non-empty string", profilePath, index)
				}
			}
		case "instructions":
			// Checked with the prompt documents in loadPrompt; listed so the field is not
			// refused as an unknown one.
		case "tools":
			// Go only: which registered tools the profile's model sees (profile_tools.go).
			if _, err := normalizeProfileTools(value, profilePath); err != nil {
				return err
			}
		default:
			if _, supported := sessionPathFields[field]; !supported {
				return configError("%s.%s is not a supported session path or profile model setting", profilePath, field)
			}
			if _, ok := nonEmptyJSONString(value); !ok {
				return configError("%s: %s must be a non-empty string", profilePath, field)
			}
		}
	}
	if !hasModels && hasModelDefault {
		models, hasModels = []string{modelDefault}, true
	}
	if !hasModelDefault && hasModels && len(models) > 0 {
		modelDefault, hasModelDefault = models[0], true
	}
	if hasModelDefault && !containsString(models, modelDefault) {
		return configError("%s.model_default must be included in models", profilePath)
	}
	return nil
}

func nonEmptyJSONString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}
