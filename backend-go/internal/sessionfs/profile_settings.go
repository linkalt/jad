package sessionfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PathProfileInfo describes the active and available path profiles in setting.json.
type PathProfileInfo struct {
	ActiveProfile string
	PathProfiles  []string
}

// PathProfileUpdate is the persisted result of changing the active path profile.
type PathProfileUpdate struct {
	ActiveProfile string
	PathProfiles  []string
	Changed       bool
}

// ModelDefaultUpdate is the persisted model selection for one path profile.
type ModelDefaultUpdate struct {
	ActiveProfile string
	PathProfiles  []string
	ModelDefault  string
	Changed       bool
}

// ReadPathProfileInfo reads path profile metadata without resolving API credentials.
func ReadPathProfileInfo(settingPath string) (PathProfileInfo, error) {
	setting, err := readSessionObject(settingPath, true)
	if err != nil {
		return PathProfileInfo{}, err
	}
	return pathProfileInfo(setting, settingPath)
}

func pathProfileInfo(setting map[string]json.RawMessage, settingPath string) (PathProfileInfo, error) {
	paths, err := objectField(setting, "paths", settingPath)
	if err != nil {
		return PathProfileInfo{}, err
	}
	profiles, exists := paths["profiles"]
	if !exists || string(profiles) == "null" {
		return PathProfileInfo{PathProfiles: []string{}}, nil
	}
	ordered, err := orderedObjectKeys(profiles)
	if err != nil {
		return PathProfileInfo{}, configError("%s: paths.profiles must be a JSON object", settingPath)
	}
	active := ""
	if raw, exists := paths["active_profile"]; exists {
		if err := json.Unmarshal(raw, &active); err != nil {
			return PathProfileInfo{}, configError("%s: paths.active_profile must be a string", settingPath)
		}
	}
	return PathProfileInfo{ActiveProfile: active, PathProfiles: ordered}, nil
}

// SetPathProfile changes paths.active_profile and optionally pins the resolved
// storage paths for both profiles, keeping their transcript/cache ownership stable.
func SetPathProfile(settingPath, profile string, durability string, storage map[string]SessionStoragePaths) (PathProfileUpdate, error) {
	if !profileNamePattern.MatchString(profile) {
		return PathProfileUpdate{}, &ClientError{Message: "active_profile must contain only letters, numbers, underscores, or hyphens"}
	}
	setting, err := readSessionObject(settingPath, true)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	info, err := pathProfileInfo(setting, settingPath)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	if len(info.PathProfiles) == 0 {
		return PathProfileUpdate{}, &ClientError{Message: "session has no configured path profiles"}
	}
	if !containsString(info.PathProfiles, profile) {
		return PathProfileUpdate{}, &ClientError{Message: fmt.Sprintf("path profile '%s' is not defined in session setting paths.profiles", profile)}
	}
	paths, err := objectField(setting, "paths", settingPath)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	profiles, err := rawObject(paths["profiles"], settingPath+": paths.profiles")
	if err != nil {
		return PathProfileUpdate{}, err
	}
	changed := info.ActiveProfile != profile
	storageChanged := false
	for profileName, resolved := range storage {
		if !containsString(info.PathProfiles, profileName) {
			return PathProfileUpdate{}, &ClientError{Message: fmt.Sprintf("path profile '%s' is not defined in session setting paths.profiles", profileName)}
		}
		profileRaw, exists := profiles[profileName]
		if !exists {
			return PathProfileUpdate{}, &ClientError{Message: fmt.Sprintf("path profile '%s' is not defined in session setting paths.profiles", profileName)}
		}
		profileFields, err := rawObject(profileRaw, settingPath+": paths.profiles."+profileName)
		if err != nil {
			return PathProfileUpdate{}, err
		}
		for name, value := range resolved.pathFields() {
			encoded, err := json.Marshal(value)
			if err != nil {
				return PathProfileUpdate{}, err
			}
			if !rawJSONEqual(profileFields[name], encoded) {
				profileFields[name] = encoded
				storageChanged = true
			}
		}
		encoded, err := json.Marshal(profileFields)
		if err != nil {
			return PathProfileUpdate{}, err
		}
		profiles[profileName] = encoded
	}
	if changed || storageChanged {
		profilesJSON, err := marshalOrderedObject(profiles, info.PathProfiles)
		if err != nil {
			return PathProfileUpdate{}, err
		}
		paths["profiles"] = profilesJSON
		if changed {
			paths["active_profile"], err = json.Marshal(profile)
			if err != nil {
				return PathProfileUpdate{}, err
			}
		}
		pathsJSON, err := json.Marshal(paths)
		if err != nil {
			return PathProfileUpdate{}, err
		}
		setting["paths"] = pathsJSON
		if err := writeSessionSettingsAtomic(settingPath, setting, durability == "strict"); err != nil {
			return PathProfileUpdate{}, fmt.Errorf("write session path profile settings: %w", err)
		}
	}
	return PathProfileUpdate{ActiveProfile: profile, PathProfiles: info.PathProfiles, Changed: changed}, nil
}

// SetModelDefault writes model_default in the named profile after verifying it
// is one of that profile's configured model paths.
func SetModelDefault(settingPath, profile, modelPath, durability string) (ModelDefaultUpdate, error) {
	if !profileNamePattern.MatchString(profile) {
		return ModelDefaultUpdate{}, &ClientError{Message: "profile must contain only letters, numbers, underscores, or hyphens"}
	}
	if strings.TrimSpace(modelPath) == "" {
		return ModelDefaultUpdate{}, &ClientError{Message: "model_default must be a non-empty string"}
	}
	modelPath = strings.TrimSpace(modelPath)
	setting, err := readSessionObject(settingPath, true)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	info, err := pathProfileInfo(setting, settingPath)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	if len(info.PathProfiles) == 0 {
		return ModelDefaultUpdate{}, &ClientError{Message: "session has no configured path profiles"}
	}
	if !containsString(info.PathProfiles, profile) {
		return ModelDefaultUpdate{}, &ClientError{Message: fmt.Sprintf("path profile '%s' is not defined in session setting paths.profiles", profile)}
	}
	paths, err := objectField(setting, "paths", settingPath)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	profiles, err := rawObject(paths["profiles"], settingPath+": paths.profiles")
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	profileFields, err := rawObject(profiles[profile], settingPath+": paths.profiles."+profile)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	models, err := configuredProfileModels(profileFields, settingPath, profile)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	if !containsString(models, modelPath) {
		return ModelDefaultUpdate{}, &ClientError{Message: "model config path is not available in the active session path profile"}
	}
	current := ""
	if raw, exists := profileFields["model_default"]; exists {
		if err := json.Unmarshal(raw, &current); err != nil {
			return ModelDefaultUpdate{}, configError("%s: paths.profiles.%s.model_default must be a non-empty string", settingPath, profile)
		}
	}
	changed := current != modelPath
	if changed {
		profileFields["model_default"], err = json.Marshal(modelPath)
		if err != nil {
			return ModelDefaultUpdate{}, err
		}
		profiles[profile], err = json.Marshal(profileFields)
		if err != nil {
			return ModelDefaultUpdate{}, err
		}
		paths["profiles"], err = marshalOrderedObject(profiles, info.PathProfiles)
		if err != nil {
			return ModelDefaultUpdate{}, err
		}
		setting["paths"], err = json.Marshal(paths)
		if err != nil {
			return ModelDefaultUpdate{}, err
		}
		if err := writeSessionSettingsAtomic(settingPath, setting, durability == "strict"); err != nil {
			return ModelDefaultUpdate{}, fmt.Errorf("write session model settings: %w", err)
		}
	}
	return ModelDefaultUpdate{ActiveProfile: profile, PathProfiles: info.PathProfiles, ModelDefault: modelPath, Changed: changed}, nil
}

func configuredProfileModels(fields map[string]json.RawMessage, settingPath, profile string) ([]string, error) {
	if raw, exists := fields["models"]; exists {
		var models []string
		if err := json.Unmarshal(raw, &models); err != nil || models == nil {
			return nil, configError("%s: paths.profiles.%s.models must be an array", settingPath, profile)
		}
		for index, model := range models {
			models[index] = strings.TrimSpace(model)
			if models[index] == "" {
				return nil, configError("%s: paths.profiles.%s.models[%d] must be a non-empty string", settingPath, profile, index)
			}
		}
		return models, nil
	}
	if raw, exists := fields["model_default"]; exists {
		var model string
		if err := json.Unmarshal(raw, &model); err != nil || strings.TrimSpace(model) == "" {
			return nil, configError("%s: paths.profiles.%s.model_default must be a non-empty string", settingPath, profile)
		}
		return []string{strings.TrimSpace(model)}, nil
	}
	return []string{}, nil
}

func rawObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, configError("%s must be a JSON object", path)
	}
	return fields, nil
}

func rawJSONEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && fmt.Sprint(leftValue) == fmt.Sprint(rightValue)
}

func (p SessionStoragePaths) pathFields() map[string]string {
	return map[string]string{
		"messages":         p.Messages,
		"messages_backup":  p.MessagesBackup,
		"compaction_cache": p.CompactionCache,
		"history_cache":    p.HistoryCache,
		"usage_cache":      p.UsageCache,
		"cursor_cache":     p.CursorCache,
		"run_error_cache":  p.RunErrorCache,
		"statecontext":     p.StateContext,
		"staging":          p.Staging,
		"http_debug":       p.HTTPDebug,
	}
}

func marshalOrderedObject(fields map[string]json.RawMessage, preferredOrder []string) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('{')
	written := make(map[string]struct{}, len(fields))
	first := true
	appendField := func(name string, value json.RawMessage) error {
		encodedName, err := json.Marshal(name)
		if err != nil {
			return err
		}
		if !first {
			output.WriteByte(',')
		}
		first = false
		output.Write(encodedName)
		output.WriteByte(':')
		output.Write(value)
		written[name] = struct{}{}
		return nil
	}
	for _, name := range preferredOrder {
		if value, exists := fields[name]; exists {
			if err := appendField(name, value); err != nil {
				return nil, err
			}
		}
	}
	remainder := make([]string, 0, len(fields)-len(written))
	for name := range fields {
		if _, exists := written[name]; !exists {
			remainder = append(remainder, name)
		}
	}
	sort.Strings(remainder)
	for _, name := range remainder {
		if err := appendField(name, fields[name]); err != nil {
			return nil, err
		}
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func writeSessionSettingsAtomic(path string, setting map[string]json.RawMessage, syncWrite bool) (result error) {
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if syncWrite {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
