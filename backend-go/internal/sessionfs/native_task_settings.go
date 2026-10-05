package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	defaultNativeTaskStorageDir         = ".tasks"
	defaultNativeTaskMaxTasks     int64 = 1_000
	defaultNativeTaskListLimit    int64 = 100
	defaultNativeTaskMaxListLimit int64 = 500
	defaultNativeTaskMaxTitle     int64 = 500
	defaultNativeTaskMaxTags      int64 = 32
	defaultNativeTaskMaxTagChars  int64 = 50
	defaultNativeTaskMaxDocuments int64 = 32
	defaultNativeTaskMaxDocChars  int64 = 1_024
	defaultNativeTaskStorageBytes int64 = 1 * 1024 * 1024
	defaultNativeTaskListBytes    int64 = 64 * 1024
	maxNativeTaskMaxTasks         int64 = 100_000
	maxNativeTaskMaxListLimit     int64 = 10_000
	maxNativeTaskMaxTitleChars    int64 = 10_000
	maxNativeTaskMaxTags          int64 = 1_000
	maxNativeTaskMaxTagChars      int64 = 1_000
	maxNativeTaskMaxDocuments     int64 = 1_000
	maxNativeTaskMaxDocChars      int64 = 16_384
	maxNativeTaskBytes            int64 = 10 * 1024 * 1024
	defaultNativeTaskStateLimit   int64 = 25
	maxNativeTaskStateContext     int64 = 100_000
)

var defaultNativeTaskStateStatuses = []string{"in-progress", "done"}
var allowedNativeTaskStatuses = map[string]struct{}{
	"todo": {}, "in-progress": {}, "blocked": {}, "done": {},
}

// NativeTaskStateContextSettings bounds task records copied into a compaction
// state-context snapshot.
type NativeTaskStateContextSettings struct {
	Statuses []string
	Limit    int64
}

// NativeTaskSettings contains persistent task storage and output limits for
// native.task.
type NativeTaskSettings struct {
	MaxTasks         int64
	ListLimit        int64
	MaxListLimit     int64
	MaxTitleChars    int64
	MaxTags          int64
	MaxTagChars      int64
	MaxDocuments     int64
	MaxDocumentChars int64
	MaxStorageBytes  int64
	MaxListBytes     int64
	StorageDir       string
	StateContext     NativeTaskStateContextSettings
}

func normalizeNativeTaskSettings(setting map[string]json.RawMessage, settingPath, cwd, toolStorageDir string) (*NativeTaskSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	taskRaw, exists := nativeFields["task"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(taskRaw, settingPath+": tools.native.task")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key == "storage_path" {
			return nil, configError("%s: tools.native.task.storage_path was removed; use storage_dir", settingPath)
		}
		switch key {
		case "max_tasks", "list_limit", "max_list_limit", "max_title_chars", "max_tags", "max_tag_chars", "max_documents", "max_document_chars", "max_storage_bytes", "max_list_bytes", "storage_dir", "statecontext":
		default:
			return nil, configError("%s: tools.native.task.%s is not supported", settingPath, key)
		}
	}

	settings := &NativeTaskSettings{}
	if settings.MaxTasks, err = configuredInteger(fields, "max_tasks", defaultNativeTaskMaxTasks, 1, maxNativeTaskMaxTasks, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxListLimit, err = configuredInteger(fields, "max_list_limit", defaultNativeTaskMaxListLimit, 1, maxNativeTaskMaxListLimit, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.ListLimit, err = configuredInteger(fields, "list_limit", defaultNativeTaskListLimit, 1, maxNativeTaskMaxListLimit, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.ListLimit > settings.MaxListLimit {
		return nil, configError("%s: tools.native.task.list_limit must not exceed max_list_limit", settingPath)
	}
	if settings.MaxTitleChars, err = configuredInteger(fields, "max_title_chars", defaultNativeTaskMaxTitle, 1, maxNativeTaskMaxTitleChars, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxTags, err = configuredInteger(fields, "max_tags", defaultNativeTaskMaxTags, 1, maxNativeTaskMaxTags, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxTagChars, err = configuredInteger(fields, "max_tag_chars", defaultNativeTaskMaxTagChars, 1, maxNativeTaskMaxTagChars, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxDocuments, err = configuredInteger(fields, "max_documents", defaultNativeTaskMaxDocuments, 1, maxNativeTaskMaxDocuments, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxDocumentChars, err = configuredInteger(fields, "max_document_chars", defaultNativeTaskMaxDocChars, 1, maxNativeTaskMaxDocChars, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxStorageBytes, err = configuredInteger(fields, "max_storage_bytes", defaultNativeTaskStorageBytes, 1, maxNativeTaskBytes, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	if settings.MaxListBytes, err = configuredInteger(fields, "max_list_bytes", defaultNativeTaskListBytes, 1, maxNativeTaskBytes, settingPath, "tools.native.task"); err != nil {
		return nil, err
	}
	settings.StorageDir, err = normalizeNativeTaskStorageDir(fields, cwd, toolStorageDir, settingPath)
	if err != nil {
		return nil, err
	}
	settings.StateContext, err = normalizeNativeTaskStateContext(fields["statecontext"], settingPath, settings.MaxListLimit)
	if err != nil {
		return nil, err
	}
	return settings, nil
}

func normalizeNativeTaskStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeTaskStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.task.storage_dir must be a non-empty string", settingPath)
		}
		configured = strings.TrimSpace(configured)
	}
	configured = expandHome(configured)
	if !filepath.IsAbs(configured) {
		base := cwd
		if toolStorageDir != "" {
			base = toolStorageDir
		}
		configured = filepath.Join(base, configured)
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve tools.native.task.storage_dir: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func normalizeNativeTaskStateContext(raw json.RawMessage, settingPath string, maxLimit int64) (NativeTaskStateContextSettings, error) {
	settings := NativeTaskStateContextSettings{Statuses: append([]string(nil), defaultNativeTaskStateStatuses...), Limit: min(defaultNativeTaskStateLimit, maxLimit)}
	if len(raw) == 0 {
		return settings, nil
	}
	section := settingPath + ": tools.native.task.statecontext"
	fields, err := decodeConfigObject(raw, section)
	if err != nil {
		return NativeTaskStateContextSettings{}, configError("%s must be an object", section)
	}
	if value, exists := fields["statuses"]; exists && strings.TrimSpace(string(value)) != "null" {
		var rawStatuses []json.RawMessage
		if err := json.Unmarshal(value, &rawStatuses); err != nil || rawStatuses == nil {
			return NativeTaskStateContextSettings{}, configError("%s.statuses must contain task statuses", section)
		}
		statuses := make([]string, 0, len(rawStatuses))
		seen := make(map[string]struct{}, len(rawStatuses))
		for _, rawStatus := range rawStatuses {
			var status string
			if err := json.Unmarshal(rawStatus, &status); err != nil {
				return NativeTaskStateContextSettings{}, configError("%s.statuses must contain task statuses", section)
			}
			if _, ok := allowedNativeTaskStatuses[status]; !ok {
				return NativeTaskStateContextSettings{}, configError("%s.statuses must contain task statuses", section)
			}
			if _, exists := seen[status]; !exists {
				statuses = append(statuses, status)
				seen[status] = struct{}{}
			}
		}
		settings.Statuses = statuses
	}
	if settings.Limit, err = configuredInteger(fields, "limit", min(defaultNativeTaskStateLimit, maxLimit), 1, maxLimit, settingPath, "tools.native.task.statecontext"); err != nil {
		return NativeTaskStateContextSettings{}, err
	}
	return settings, nil
}
