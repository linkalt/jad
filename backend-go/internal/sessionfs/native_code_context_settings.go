package sessionfs

import (
	"encoding/json"
	"strings"
)

const (
	defaultNativeCodeContextStorageDir                = ".code_context"
	defaultNativeCodeContextMaxEntries          int64 = 500
	maxNativeCodeContextMaxEntries              int64 = 100_000
	defaultNativeCodeContextMaxDescriptionChars int64 = 4_000
	maxNativeCodeContextMaxDescriptionChars     int64 = 100_000
	defaultNativeCodeContextMaxRangeLines       int64 = 2_000
	maxNativeCodeContextMaxRangeLines           int64 = 1_000_000
	defaultNativeCodeContextMaxFileBytes        int64 = 4 * 1024 * 1024
	// defaultNativeCodeContextMaxOutputBytes is about 100K tokens of code: the ceiling of
	// one get, not a target.
	defaultNativeCodeContextMaxOutputBytes  int64 = 400_000
	defaultNativeCodeContextMaxStorageBytes int64 = 1 * 1024 * 1024
	// defaultNativeCodeContextDefaultPriority is the priority of a range read without one:
	// secondary, until the caller ranks it.
	defaultNativeCodeContextDefaultPriority int64 = 6
	defaultNativeCodeContextMaxFindResults  int64 = 20
	maxNativeCodeContextMaxFindResults      int64 = 1_000
	defaultNativeCodeContextMaxScanTasks    int64 = 200
	maxNativeCodeContextMaxScanTasks        int64 = 10_000
	defaultNativeCodeContextMaxBatchItems   int64 = 50
	maxNativeCodeContextMaxBatchItems       int64 = 1_000
)

var nativeCodeContextKeys = []string{"storage_dir", "paths", "max_entries", "max_description_chars", "max_range_lines", "max_file_bytes", "max_output_bytes", "max_storage_bytes", "relocate", "default_priority", "max_find_results", "max_scan_tasks", "max_batch_items"}

// NativeCodeContextSettings configure native.code_context: a per-task list of exact
// source ranges, each with a description, that get reads back as code.
type NativeCodeContextSettings struct {
	// StorageDir holds one JSON file per task. A configured relative directory is from the
	// cwd; the default is under the profile's tool_storage (the cwd without a profile). An
	// absolute directory, or the same relative one, shares it between profiles.
	StorageDir string
	// Policy is the tool's own read rules: which files a range may name.
	Policy NativePathPolicy
	// MaxEntries bounds the entries of one task.
	MaxEntries          int64
	MaxDescriptionChars int64
	// MaxRangeLines bounds end-start+1 of one entry.
	MaxRangeLines int64
	// MaxFileBytes is the largest file a range may be read from.
	MaxFileBytes int64
	// MaxOutputBytes bounds the code of one get; entries of the highest priority numbers
	// are left out first.
	MaxOutputBytes  int64
	MaxStorageBytes int64
	// Relocate moves a range whose saved bytes are no longer at its lines but occur
	// exactly once elsewhere in the file to where they are now.
	Relocate bool
	// DefaultPriority is the priority of a range saved by read without one.
	DefaultPriority int64
	// MaxFindResults bounds the entries one find returns.
	MaxFindResults int64
	// MaxScanTasks bounds the tasks a call without a task lists, most recently written
	// first.
	MaxScanTasks int64
	// MaxBatchItems bounds the items of one read or update.
	MaxBatchItems int64
}

func normalizeNativeCodeContextSettings(setting map[string]json.RawMessage, globalJadRoot, settingPath, cwd, toolStorageDir string) (*NativeCodeContextSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil || !exists {
		return nil, err
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["code_context"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.code_context"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeCodeContextKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeCodeContextKeys, "")
		}
	}
	settings := &NativeCodeContextSettings{Policy: NativePathPolicy{Paths: []NativePathRule{}}}
	if rawPaths, exists := fields["paths"]; exists {
		if settings.Policy, err = normalizeNativePathPolicy(rawPaths, globalJadRoot, settingPath+": "+section+".paths"); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		name               string
		target             *int64
		fallback, min, max int64
	}{
		{"max_entries", &settings.MaxEntries, defaultNativeCodeContextMaxEntries, 1, maxNativeCodeContextMaxEntries},
		{"max_description_chars", &settings.MaxDescriptionChars, defaultNativeCodeContextMaxDescriptionChars, 1, maxNativeCodeContextMaxDescriptionChars},
		{"max_range_lines", &settings.MaxRangeLines, defaultNativeCodeContextMaxRangeLines, 1, maxNativeCodeContextMaxRangeLines},
		{"max_file_bytes", &settings.MaxFileBytes, defaultNativeCodeContextMaxFileBytes, 1, maxNativeToolBytes},
		{"max_output_bytes", &settings.MaxOutputBytes, defaultNativeCodeContextMaxOutputBytes, 1, maxNativeToolBytes},
		{"max_storage_bytes", &settings.MaxStorageBytes, defaultNativeCodeContextMaxStorageBytes, 1, maxNativeToolBytes},
		{"default_priority", &settings.DefaultPriority, defaultNativeCodeContextDefaultPriority, 1, 6},
		{"max_find_results", &settings.MaxFindResults, defaultNativeCodeContextMaxFindResults, 1, maxNativeCodeContextMaxFindResults},
		{"max_scan_tasks", &settings.MaxScanTasks, defaultNativeCodeContextMaxScanTasks, 1, maxNativeCodeContextMaxScanTasks},
		{"max_batch_items", &settings.MaxBatchItems, defaultNativeCodeContextMaxBatchItems, 1, maxNativeCodeContextMaxBatchItems},
	} {
		if *field.target, err = configuredInteger(fields, field.name, field.fallback, field.min, field.max, settingPath, section); err != nil {
			return nil, err
		}
	}
	settings.Relocate = true
	if raw, exists := fields["relocate"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &settings.Relocate); err != nil {
			return nil, configError("%s: %s.relocate must be a boolean", settingPath, section)
		}
	}
	if settings.StorageDir, err = normalizeNativeCodeContextStorageDir(fields, cwd, toolStorageDir, settingPath); err != nil {
		return nil, err
	}
	return settings, nil
}

// normalizeNativeCodeContextStorageDir follows the task tool: see normalizeToolStorageSubdir.
func normalizeNativeCodeContextStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	return normalizeToolStorageSubdir(fields, defaultNativeCodeContextStorageDir, cwd, toolStorageDir, "tools.native.code_context.storage_dir", settingPath)
}
