package sessionfs

import (
	"encoding/json"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	maxSessionFileDepth   = 64
	maxSessionFileEntries = 1_000_000
	maxSessionFileResults = 100_000
)

// SessionFileListingSettings bounds the read-only file list offered by the UI.
type SessionFileListingSettings struct {
	Enabled       bool
	MaxDepth      int
	MaxEntries    int
	MaxResults    int
	IncludeHidden bool
	// IncludeDirectories also lists each readable folder, as its path with a trailing slash.
	IncludeDirectories bool
	IgnoreDirectories  []string
}

// NativePathRule is one resolved allow/deny path rule for native read access.
type NativePathRule struct {
	Action string
	Path   string
}

// NativePathPolicy carries the path rules used to filter session file listings.
type NativePathPolicy struct {
	Paths             []NativePathRule
	UnrestrictedPaths bool
	// Within, when not empty, is a second condition on top of Paths: a path must also
	// lie inside one of these absolute roots. It can only narrow what Paths allow.
	Within []string
}

func defaultSessionFileListingSettings() SessionFileListingSettings {
	return SessionFileListingSettings{
		Enabled:            true,
		IncludeDirectories: true,
		MaxDepth:           12,
		MaxEntries:         5000,
		MaxResults:         2000,
		IgnoreDirectories: []string{
			"node_modules", ".git", "dist", "build", "coverage", ".next",
			"__pycache__", ".venv", "venv", ".cache",
		},
	}
}

func normalizeSessionFileListing(raw json.RawMessage, path string) (SessionFileListingSettings, error) {
	settings := defaultSessionFileListingSettings()
	if len(raw) == 0 {
		return settings, nil
	}
	fields, err := decodeConfigObject(raw, path+": session_files")
	if err != nil {
		return SessionFileListingSettings{}, err
	}
	if err := rejectUnknownKeys(fields, path+": session_files", []string{"enabled", "include_hidden", "include_directories", "max_depth", "max_entries", "max_results", "ignore_directories"}); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.Enabled, err = sessionFileBool(fields, "enabled", settings.Enabled, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.IncludeHidden, err = sessionFileBool(fields, "include_hidden", settings.IncludeHidden, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.IncludeDirectories, err = sessionFileBool(fields, "include_directories", settings.IncludeDirectories, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.MaxDepth, err = sessionFileInteger(fields, "max_depth", settings.MaxDepth, maxSessionFileDepth, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.MaxEntries, err = sessionFileInteger(fields, "max_entries", settings.MaxEntries, maxSessionFileEntries, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if settings.MaxResults, err = sessionFileInteger(fields, "max_results", settings.MaxResults, maxSessionFileResults, path); err != nil {
		return SessionFileListingSettings{}, err
	}
	if rawIgnore, ok := fields["ignore_directories"]; ok {
		var values []json.RawMessage
		if err := json.Unmarshal(rawIgnore, &values); err != nil || values == nil {
			return SessionFileListingSettings{}, configError("%s: session_files.ignore_directories must be an array", path)
		}
		settings.IgnoreDirectories = make([]string, 0, len(values))
		for index, rawValue := range values {
			value, err := sessionFileString(rawValue)
			if err != nil || strings.TrimSpace(value) == "" {
				return SessionFileListingSettings{}, configError("%s: session_files.ignore_directories[%d] must be a non-empty string", path, index)
			}
			settings.IgnoreDirectories = append(settings.IgnoreDirectories, strings.TrimSpace(value))
		}
	}
	return settings, nil
}

func normalizeNativeReadPolicy(setting map[string]json.RawMessage, globalJadRoot, path string) (NativePathPolicy, error) {
	policy := NativePathPolicy{Paths: []NativePathRule{}}
	nativeRaw, exists, err := nativeEnvelope(setting, path)
	if err != nil {
		return NativePathPolicy{}, err
	}
	if !exists {
		return policy, nil
	}
	native, err := decodeConfigObject(nativeRaw, path+": tools.native")
	if err != nil {
		return NativePathPolicy{}, err
	}
	readRaw, exists := native["read"]
	if !exists {
		return policy, nil
	}
	read, err := decodeConfigObject(readRaw, path+": tools.native.read")
	if err != nil {
		return NativePathPolicy{}, err
	}
	pathsRaw, exists := read["paths"]
	if !exists {
		return policy, nil
	}
	return normalizeNativePathPolicy(pathsRaw, globalJadRoot, path+": tools.native.read.paths")
}

func normalizeNativePathPolicy(raw json.RawMessage, globalJadRoot, path string) (NativePathPolicy, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return NativePathPolicy{}, configError("%s must be an array or an object with allow/deny lists", path)
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return NativePathPolicy{}, configError("%s must be an array of path rules", path)
		}
		rules := make([]NativePathRule, 0, len(values))
		for index, rawRule := range values {
			fields, err := decodeConfigObject(rawRule, path+"[index]")
			if err != nil {
				return NativePathPolicy{}, configError("%s[%d] must be an object", path, index)
			}
			action, actionErr := sessionFileString(fields["action"])
			configuredPath, pathErr := sessionFileString(fields["path"])
			if actionErr != nil || (action != "allow" && action != "deny") || pathErr != nil || strings.TrimSpace(configuredPath) == "" {
				return NativePathPolicy{}, configError("%s[%d] must contain action allow/deny and a non-empty path", path, index)
			}
			rules = append(rules, NativePathRule{Action: action, Path: resolveNativeRulePath(configuredPath, globalJadRoot)})
		}
		return NativePathPolicy{Paths: rules}, nil
	}

	fields, err := decodeConfigObject(raw, path)
	if err != nil {
		return NativePathPolicy{}, configError("%s must be an array of path rules or an object with allow/deny lists", path)
	}
	for name := range fields {
		if name != "allow" && name != "deny" {
			return NativePathPolicy{}, configError("%s.%s is not supported; use allow and deny", path, name)
		}
	}
	allow, err := normalizeNativePathRuleList(fields["allow"], "allow", path+".allow", globalJadRoot)
	if err != nil {
		return NativePathPolicy{}, err
	}
	deny, err := normalizeNativePathRuleList(fields["deny"], "deny", path+".deny", globalJadRoot)
	if err != nil {
		return NativePathPolicy{}, err
	}
	rules := append(allow, deny...)
	return NativePathPolicy{Paths: rules, UnrestrictedPaths: len(allow) == 0}, nil
}

func normalizeNativePathRuleList(raw json.RawMessage, action, path, globalJadRoot string) ([]NativePathRule, error) {
	if len(raw) == 0 {
		return []NativePathRule{}, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return nil, configError("%s must be a path string or an array of path strings", path)
	}
	values := []json.RawMessage{raw}
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return nil, configError("%s must be a path string or an array of path strings", path)
		}
	}
	rules := make([]NativePathRule, 0, len(values))
	for index, rawValue := range values {
		configuredPath, err := sessionFileString(rawValue)
		if err != nil {
			fields, objectErr := decodeConfigObject(rawValue, path)
			if objectErr != nil {
				return nil, configError("%s[%d] must be a path string or an object with a path", path, index)
			}
			configuredPath, err = sessionFileString(fields["path"])
		}
		if err != nil || strings.TrimSpace(configuredPath) == "" {
			return nil, configError("%s[%d] must be a non-empty path", path, index)
		}
		rules = append(rules, NativePathRule{Action: action, Path: resolveNativeRulePath(configuredPath, globalJadRoot)})
	}
	return rules, nil
}

func resolveNativeRulePath(configuredPath, globalJadRoot string) string {
	value := expandHome(strings.TrimSpace(configuredPath))
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return resolveFrom(globalJadRoot, value)
}

func rejectUnknownKeys(fields map[string]json.RawMessage, fieldPath string, allowed []string) error {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !slices.Contains(allowed, key) {
			return unsupportedToolKey(fieldPath, key, allowed, "")
		}
	}
	return nil
}

func decodeConfigObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s must be a JSON object", path)
	}
	return fields, nil
}

func sessionFileBool(fields map[string]json.RawMessage, name string, fallback bool, path string) (bool, error) {
	raw, exists := fields[name]
	if !exists {
		return fallback, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, configError("%s: session_files.%s must be a boolean", path, name)
	}
	parsed, ok := value.(bool)
	if !ok {
		return false, configError("%s: session_files.%s must be a boolean", path, name)
	}
	return parsed, nil
}

func sessionFileInteger(fields map[string]json.RawMessage, name string, fallback, maximum int, path string) (int, error) {
	raw, exists := fields[name]
	if !exists {
		return fallback, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, configError("%s: session_files.%s must be a safe integer between 0 and %d", path, name, maximum)
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < 0 || number > float64(maximum) {
		return 0, configError("%s: session_files.%s must be a safe integer between 0 and %d", path, name, maximum)
	}
	return int(number), nil
}

func sessionFileString(raw json.RawMessage) (string, error) {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return "", configError("value must be a string")
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", configError("value must be a non-empty string")
	}
	return strings.TrimSpace(text), nil
}
