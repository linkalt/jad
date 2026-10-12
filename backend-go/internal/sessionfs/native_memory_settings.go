package sessionfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultNativeMemoryStorageDir         = ".memory"
	defaultNativeMemoryMaxTypes     int64 = 64
	defaultNativeMemoryMaxRecords   int64 = 1_000
	defaultNativeMemoryMaxTotal     int64 = 10_000
	defaultNativeMemoryRecordBytes  int64 = 256 * 1024
	defaultNativeMemoryTotalBytes   int64 = 64 * 1024 * 1024
	defaultNativeMemoryListLimit    int64 = 20
	defaultNativeMemoryMaxListLimit int64 = 100
	defaultNativeMemoryListBytes    int64 = 64 * 1024
	defaultNativeMemoryScanRecords  int64 = 2_000
	defaultNativeMemoryScanBytes    int64 = 8 * 1024 * 1024
	defaultNativeMemoryFilterFields int64 = 8
	defaultNativeMemoryFilterDepth  int64 = 8
	defaultNativeMemoryJSONDepth    int64 = 32
	defaultNativeMemoryMaxCursors   int64 = 1_000
	defaultNativeMemoryCursorTTL    int64 = 24 * 60 * 60 * 1000
	defaultNativeMemoryCursorBytes  int64 = 64 * 1024
	defaultNativeMemoryStateLimit   int64 = 10
	maxNativeMemoryRecords          int64 = 100_000
	maxNativeMemoryTotalRecords     int64 = 10_000_000
	maxNativeMemoryBytes            int64 = 10 * 1024 * 1024
	maxNativeMemoryTotalBytes       int64 = maxNativeMemoryBytes * 100
	maxNativeMemoryFilterFields     int64 = 64
	maxNativeMemoryFilterDepth      int64 = 32
	maxNativeMemoryJSONDepth        int64 = 128
	maxNativeMemoryCursorTTL        int64 = 30 * 24 * 60 * 60 * 1000
)

var (
	nativeMemoryTypePattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	nativeMemoryFilterKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// Parts must match what the memory store can read at query time.
	nativeMemoryGroupByPartPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// NativeMemoryStateContextSelector selects a bounded set of records for a
// compaction snapshot. ExcludeAny contains clauses that exclude a record when
// every key in a clause matches. A non-empty GroupBy replaces the records with
// a count per value at that field path; Limit is then unused.
type NativeMemoryStateContextSelector struct {
	Type       string
	Filter     map[string]any
	ExcludeAny []map[string]any
	Limit      int64
	GroupBy    string
}

// NativeMemorySettings contains storage, query, and state-context bounds for
// native.memory.
type NativeMemorySettings struct {
	StorageDir        string
	StateContext      []NativeMemoryStateContextSelector
	MaxTypes          int64
	MaxRecordsPerType int64
	MaxTotalRecords   int64
	MaxRecordBytes    int64
	MaxTotalBytes     int64
	ListLimit         int64
	MaxListLimit      int64
	MaxListBytes      int64
	MaxScanRecords    int64
	MaxScanBytes      int64
	MaxFilterFields   int64
	MaxFilterDepth    int64
	MaxJSONDepth      int64
	MaxCursors        int64
	CursorTTLMS       int64
	MaxCursorBytes    int64
	AllowedTypes      []string
}

func normalizeNativeMemorySettings(setting map[string]json.RawMessage, settingPath, cwd, toolStorageDir string) (*NativeMemorySettings, error) {
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
	memoryRaw, exists := nativeFields["memory"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(memoryRaw, settingPath+": tools.native.memory")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key == "storage_path" {
			return nil, configError("%s: tools.native.memory.storage_path was removed; use storage_dir", settingPath)
		}
		switch key {
		case "storage_dir", "statecontext", "max_types", "max_records_per_type", "max_total_records", "max_record_bytes", "max_total_bytes", "list_limit", "max_list_limit", "max_list_bytes", "max_scan_records", "max_scan_bytes", "max_filter_fields", "max_filter_depth", "max_json_depth", "max_cursors", "cursor_ttl_ms", "max_cursor_bytes", "allowed_types":
		default:
			return nil, configError("%s: tools.native.memory.%s is not supported", settingPath, key)
		}
	}

	settings := &NativeMemorySettings{}
	if settings.MaxTypes, err = configuredInteger(fields, "max_types", defaultNativeMemoryMaxTypes, 1, maxNativeMemoryRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxRecordsPerType, err = configuredInteger(fields, "max_records_per_type", defaultNativeMemoryMaxRecords, 1, maxNativeMemoryRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxTotalRecords, err = configuredInteger(fields, "max_total_records", defaultNativeMemoryMaxTotal, 1, maxNativeMemoryTotalRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxRecordBytes, err = configuredInteger(fields, "max_record_bytes", defaultNativeMemoryRecordBytes, 1, maxNativeMemoryBytes, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxTotalBytes, err = configuredInteger(fields, "max_total_bytes", defaultNativeMemoryTotalBytes, 1, maxNativeMemoryTotalBytes, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxListLimit, err = configuredInteger(fields, "max_list_limit", defaultNativeMemoryMaxListLimit, 1, maxNativeMemoryRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.ListLimit, err = configuredInteger(fields, "list_limit", defaultNativeMemoryListLimit, 1, defaultNativeMemoryMaxListLimit, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxListBytes, err = configuredInteger(fields, "max_list_bytes", defaultNativeMemoryListBytes, 1, maxNativeMemoryBytes, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxScanRecords, err = configuredInteger(fields, "max_scan_records", defaultNativeMemoryScanRecords, 1, maxNativeMemoryTotalRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxScanBytes, err = configuredInteger(fields, "max_scan_bytes", defaultNativeMemoryScanBytes, 1, maxNativeMemoryTotalBytes, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxFilterFields, err = configuredInteger(fields, "max_filter_fields", defaultNativeMemoryFilterFields, 1, maxNativeMemoryFilterFields, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxFilterDepth, err = configuredInteger(fields, "max_filter_depth", defaultNativeMemoryFilterDepth, 1, maxNativeMemoryFilterDepth, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxJSONDepth, err = configuredInteger(fields, "max_json_depth", defaultNativeMemoryJSONDepth, 1, maxNativeMemoryJSONDepth, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxCursors, err = configuredInteger(fields, "max_cursors", defaultNativeMemoryMaxCursors, 1, maxNativeMemoryRecords, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.CursorTTLMS, err = configuredInteger(fields, "cursor_ttl_ms", defaultNativeMemoryCursorTTL, 1, maxNativeMemoryCursorTTL, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.MaxCursorBytes, err = configuredInteger(fields, "max_cursor_bytes", defaultNativeMemoryCursorBytes, 1, maxNativeMemoryBytes, settingPath, "tools.native.memory"); err != nil {
		return nil, err
	}
	if settings.ListLimit > settings.MaxListLimit || settings.MaxRecordBytes > settings.MaxTotalBytes {
		return nil, configError("%s: tools.native.memory limits are incompatible", settingPath)
	}
	settings.StorageDir, err = normalizeNativeMemoryStorageDir(fields, cwd, toolStorageDir, settingPath)
	if err != nil {
		return nil, err
	}
	if raw, exists := fields["allowed_types"]; exists {
		var allowed []string
		if err := json.Unmarshal(raw, &allowed); err != nil || allowed == nil {
			return nil, configError("%s: tools.native.memory.allowed_types must contain safe type names", settingPath)
		}
		seen := make(map[string]struct{}, len(allowed))
		settings.AllowedTypes = make([]string, 0, len(allowed))
		for _, name := range allowed {
			if !nativeMemoryTypePattern.MatchString(name) {
				return nil, configError("%s: tools.native.memory.allowed_types must contain safe type names", settingPath)
			}
			if _, exists := seen[name]; !exists {
				settings.AllowedTypes = append(settings.AllowedTypes, name)
				seen[name] = struct{}{}
			}
		}
	}
	settings.StateContext, err = normalizeNativeMemoryStateContext(fields["statecontext"], settingPath, settings.MaxListLimit, settings.MaxFilterFields, settings.MaxFilterDepth, settings.AllowedTypes)
	if err != nil {
		return nil, err
	}
	return settings, nil
}

func normalizeNativeMemoryStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	configured := defaultNativeMemoryStorageDir
	if raw, exists := fields["storage_dir"]; exists && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return "", configError("%s: tools.native.memory.storage_dir must be a non-empty string", settingPath)
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
		return "", fmt.Errorf("resolve tools.native.memory.storage_dir: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func normalizeNativeMemoryStateContext(raw json.RawMessage, settingPath string, maxLimit, maxFilterFields, maxFilterDepth int64, allowedTypes []string) ([]NativeMemoryStateContextSelector, error) {
	if len(raw) == 0 {
		return []NativeMemoryStateContextSelector{}, nil
	}
	section := settingPath + ": tools.native.memory.statecontext"
	var rawSelectors []json.RawMessage
	if err := json.Unmarshal(raw, &rawSelectors); err != nil || rawSelectors == nil || len(rawSelectors) > 128 {
		return nil, configError("%s must be an array of at most 128 {type, filter?, limit} selectors", section)
	}
	allowed := map[string]struct{}{}
	if allowedTypes != nil {
		for _, name := range allowedTypes {
			allowed[name] = struct{}{}
		}
	}
	selectors := make([]NativeMemoryStateContextSelector, 0, len(rawSelectors))
	for index, rawSelector := range rawSelectors {
		entryPath := fmt.Sprintf("%s[%d]", section, index)
		fields, err := decodeConfigObject(rawSelector, entryPath)
		if err != nil {
			return nil, err
		}
		var name string
		rawType, exists := fields["type"]
		if !exists || json.Unmarshal(rawType, &name) != nil || strings.TrimSpace(name) == "" {
			return nil, configError("%s: type must be a non-empty string", entryPath)
		}
		name = strings.TrimSpace(name)
		if !nativeMemoryTypePattern.MatchString(name) {
			return nil, configError("%s.type is invalid", entryPath)
		}
		if allowedTypes != nil {
			if _, exists := allowed[name]; !exists {
				return nil, configError("%s.type is not in tools.native.memory.allowed_types", entryPath)
			}
		}
		filter := map[string]any{}
		if raw, exists := fields["filter"]; exists && strings.TrimSpace(string(raw)) != "null" {
			filter, err = decodeMemoryFilter(raw, entryPath+".filter")
			if err != nil {
				return nil, err
			}
		}
		var excludeAny []map[string]any
		if raw, exists := fields["exclude_any"]; exists && strings.TrimSpace(string(raw)) != "null" {
			var clauses []json.RawMessage
			if err := json.Unmarshal(raw, &clauses); err != nil || len(clauses) > int(maxFilterFields) {
				return nil, configError("%s.exclude_any must be an array within tools.native.memory filter limits", entryPath)
			}
			excludeAny = make([]map[string]any, 0, len(clauses))
			for clauseIndex, clause := range clauses {
				decoded, err := decodeMemoryFilter(clause, fmt.Sprintf("%s.exclude_any[%d]", entryPath, clauseIndex))
				if err != nil || len(decoded) == 0 {
					return nil, configError("%s.exclude_any[%d] must be a non-empty object", entryPath, clauseIndex)
				}
				excludeAny = append(excludeAny, decoded)
			}
		}
		filterKeys := make([]string, 0, len(filter))
		for key := range filter {
			filterKeys = append(filterKeys, key)
		}
		for _, clause := range excludeAny {
			for key := range clause {
				filterKeys = append(filterKeys, key)
			}
		}
		invalidKey := false
		for _, key := range filterKeys {
			parts := strings.Split(key, ".")
			if int64(len(parts)) > maxFilterDepth {
				invalidKey = true
				break
			}
			for _, part := range parts {
				if !nativeMemoryFilterKeyPattern.MatchString(part) {
					invalidKey = true
					break
				}
			}
			if invalidKey {
				break
			}
		}
		if int64(len(filterKeys)) > maxFilterFields || invalidKey {
			return nil, configError("%s.filter/exclude_any exceeds tools.native.memory filter limits", entryPath)
		}
		groupBy := ""
		if raw, exists := fields["group_by"]; exists && strings.TrimSpace(string(raw)) != "null" {
			if json.Unmarshal(raw, &groupBy) != nil {
				return nil, configError("%s.group_by must be a field path string", entryPath)
			}
			parts := strings.Split(groupBy, ".")
			if int64(len(parts)) > maxFilterDepth {
				return nil, configError("%s.group_by exceeds tools.native.memory filter limits", entryPath)
			}
			for _, part := range parts {
				if !nativeMemoryGroupByPartPattern.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
					return nil, configError("%s.group_by must be a dot-separated field path of identifiers", entryPath)
				}
			}
			if _, constrained := filter[groupBy]; constrained {
				return nil, configError("%s.group_by must not also be a filter key", entryPath)
			}
		}
		limit := min(defaultNativeMemoryStateLimit, maxLimit)
		if raw, exists := fields["limit"]; exists && strings.TrimSpace(string(raw)) != "null" {
			if groupBy != "" {
				return nil, configError("%s.limit cannot be combined with group_by", entryPath)
			}
			decoded, valid := configSafeInteger(raw)
			if !valid || decoded < 1 || decoded > maxLimit {
				return nil, configError("%s.limit must be a safe integer between 1 and %d", entryPath, maxLimit)
			}
			limit = decoded
		}
		selectors = append(selectors, NativeMemoryStateContextSelector{Type: name, Filter: filter, ExcludeAny: excludeAny, Limit: limit, GroupBy: groupBy})
	}
	return selectors, nil
}

func decodeMemoryFilter(raw json.RawMessage, section string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var filter map[string]any
	if err := decoder.Decode(&filter); err != nil || filter == nil {
		return nil, configError("%s must be an object", section)
	}
	return filter, nil
}
