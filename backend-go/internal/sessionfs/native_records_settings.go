package sessionfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	defaultNativeRecordsStorageDir         = ".records"
	defaultNativeRecordsMaxTypes     int64 = 64
	defaultNativeRecordsMaxRecords   int64 = 1_000
	defaultNativeRecordsMaxTotal     int64 = 10_000
	defaultNativeRecordsRecordBytes  int64 = 256 * 1024
	defaultNativeRecordsTotalBytes   int64 = 64 * 1024 * 1024
	defaultNativeRecordsListLimit    int64 = 20
	defaultNativeRecordsMaxListLimit int64 = 100
	defaultNativeRecordsListBytes    int64 = 64 * 1024
	defaultNativeRecordsScanRecords  int64 = 2_000
	defaultNativeRecordsScanBytes    int64 = 8 * 1024 * 1024
	defaultNativeRecordsFilterFields int64 = 8
	defaultNativeRecordsFilterDepth  int64 = 8
	defaultNativeRecordsJSONDepth    int64 = 32
	defaultNativeRecordsMaxCursors   int64 = 1_000
	defaultNativeRecordsCursorTTL    int64 = 24 * 60 * 60 * 1000
	defaultNativeRecordsCursorBytes  int64 = 64 * 1024
	defaultNativeRecordsStateLimit   int64 = 10
	defaultNativeRecordsQueryBytes   int64 = 1024
	maxNativeRecordsQueryBytes       int64 = 64 * 1024
	maxNativeRecordsCount            int64 = 100_000
	maxNativeRecordsTotalRecords     int64 = 10_000_000
	maxNativeRecordsBytes            int64 = 10 * 1024 * 1024
	maxNativeRecordsTotalBytes       int64 = maxNativeRecordsBytes * 100
	maxNativeRecordsFilterFields     int64 = 64
	maxNativeRecordsFilterDepth      int64 = 32
	maxNativeRecordsJSONDepth        int64 = 128
	maxNativeRecordsCursorTTL        int64 = 30 * 24 * 60 * 60 * 1000
)

var (
	nativeRecordsTypePattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	nativeRecordsFilterKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// Parts must match what the records store can read at query time.
	nativeRecordsGroupByPartPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// NativeRecordsStateContextSelector selects a bounded set of records for a
// compaction snapshot. ExcludeAny contains clauses that exclude a record when
// every key in a clause matches. A non-empty GroupBy replaces the records with
// a count per value at that field path; Limit is then unused.
type NativeRecordsStateContextSelector struct {
	Type       string
	Filter     map[string]any
	ExcludeAny []map[string]any
	Limit      int64
	GroupBy    string
}

// NativeRecordsSettings contains storage, query, and state-context bounds for
// native.records.
type NativeRecordsSettings struct {
	StorageDir        string
	StateContext      []NativeRecordsStateContextSelector
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
	// MaxQueryBytes bounds the text of a search query.
	MaxQueryBytes int64
	AllowedTypes  []string
}

func normalizeNativeRecordsSettings(setting map[string]json.RawMessage, settingPath, cwd, toolStorageDir string) (*NativeRecordsSettings, error) {
	nativeRaw, exists, err := nativeEnvelope(setting, settingPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	recordsRaw, exists := nativeFields["records"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(recordsRaw, settingPath+": tools.native.records")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key == "storage_path" {
			return nil, configError("%s: tools.native.records.storage_path was removed; use storage_dir", settingPath)
		}
		switch key {
		case "storage_dir", "statecontext", "max_types", "max_records_per_type", "max_total_records", "max_record_bytes", "max_total_bytes", "list_limit", "max_list_limit", "max_list_bytes", "max_scan_records", "max_scan_bytes", "max_filter_fields", "max_filter_depth", "max_json_depth", "max_cursors", "cursor_ttl_ms", "max_cursor_bytes", "max_query_bytes", "allowed_types":
		default:
			return nil, configError("%s: tools.native.records.%s is not supported", settingPath, key)
		}
	}

	settings := &NativeRecordsSettings{}
	if settings.MaxTypes, err = configuredInteger(fields, "max_types", defaultNativeRecordsMaxTypes, 1, maxNativeRecordsCount, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxRecordsPerType, err = configuredInteger(fields, "max_records_per_type", defaultNativeRecordsMaxRecords, 1, maxNativeRecordsCount, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxTotalRecords, err = configuredInteger(fields, "max_total_records", defaultNativeRecordsMaxTotal, 1, maxNativeRecordsTotalRecords, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxRecordBytes, err = configuredInteger(fields, "max_record_bytes", defaultNativeRecordsRecordBytes, 1, maxNativeRecordsBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxTotalBytes, err = configuredInteger(fields, "max_total_bytes", defaultNativeRecordsTotalBytes, 1, maxNativeRecordsTotalBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxListLimit, err = configuredInteger(fields, "max_list_limit", defaultNativeRecordsMaxListLimit, 1, maxNativeRecordsCount, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.ListLimit, err = configuredInteger(fields, "list_limit", defaultNativeRecordsListLimit, 1, defaultNativeRecordsMaxListLimit, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxListBytes, err = configuredInteger(fields, "max_list_bytes", defaultNativeRecordsListBytes, 1, maxNativeRecordsBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxScanRecords, err = configuredInteger(fields, "max_scan_records", defaultNativeRecordsScanRecords, 1, maxNativeRecordsTotalRecords, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxScanBytes, err = configuredInteger(fields, "max_scan_bytes", defaultNativeRecordsScanBytes, 1, maxNativeRecordsTotalBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxFilterFields, err = configuredInteger(fields, "max_filter_fields", defaultNativeRecordsFilterFields, 1, maxNativeRecordsFilterFields, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxFilterDepth, err = configuredInteger(fields, "max_filter_depth", defaultNativeRecordsFilterDepth, 1, maxNativeRecordsFilterDepth, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxJSONDepth, err = configuredInteger(fields, "max_json_depth", defaultNativeRecordsJSONDepth, 1, maxNativeRecordsJSONDepth, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxCursors, err = configuredInteger(fields, "max_cursors", defaultNativeRecordsMaxCursors, 1, maxNativeRecordsCount, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.CursorTTLMS, err = configuredInteger(fields, "cursor_ttl_ms", defaultNativeRecordsCursorTTL, 1, maxNativeRecordsCursorTTL, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxCursorBytes, err = configuredInteger(fields, "max_cursor_bytes", defaultNativeRecordsCursorBytes, 1, maxNativeRecordsBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.MaxQueryBytes, err = configuredInteger(fields, "max_query_bytes", defaultNativeRecordsQueryBytes, 1, maxNativeRecordsQueryBytes, settingPath, "tools.native.records"); err != nil {
		return nil, err
	}
	if settings.ListLimit > settings.MaxListLimit || settings.MaxRecordBytes > settings.MaxTotalBytes {
		return nil, configError("%s: tools.native.records limits are incompatible", settingPath)
	}
	settings.StorageDir, err = normalizeNativeRecordsStorageDir(fields, cwd, toolStorageDir, settingPath)
	if err != nil {
		return nil, err
	}
	if raw, exists := fields["allowed_types"]; exists {
		var allowed []string
		if err := json.Unmarshal(raw, &allowed); err != nil || allowed == nil {
			return nil, configError("%s: tools.native.records.allowed_types must contain safe type names", settingPath)
		}
		seen := make(map[string]struct{}, len(allowed))
		settings.AllowedTypes = make([]string, 0, len(allowed))
		for _, name := range allowed {
			if !nativeRecordsTypePattern.MatchString(name) {
				return nil, configError("%s: tools.native.records.allowed_types must contain safe type names", settingPath)
			}
			if _, exists := seen[name]; !exists {
				settings.AllowedTypes = append(settings.AllowedTypes, name)
				seen[name] = struct{}{}
			}
		}
	}
	settings.StateContext, err = normalizeNativeRecordsStateContext(fields["statecontext"], settingPath, settings.MaxListLimit, settings.MaxFilterFields, settings.MaxFilterDepth, settings.AllowedTypes)
	if err != nil {
		return nil, err
	}
	return settings, nil
}

func normalizeNativeRecordsStorageDir(fields map[string]json.RawMessage, cwd, toolStorageDir, settingPath string) (string, error) {
	return normalizeToolStorageSubdir(fields, defaultNativeRecordsStorageDir, cwd, toolStorageDir, "tools.native.records.storage_dir", settingPath)
}

func normalizeNativeRecordsStateContext(raw json.RawMessage, settingPath string, maxLimit, maxFilterFields, maxFilterDepth int64, allowedTypes []string) ([]NativeRecordsStateContextSelector, error) {
	if len(raw) == 0 {
		return []NativeRecordsStateContextSelector{}, nil
	}
	section := settingPath + ": tools.native.records.statecontext"
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
	selectors := make([]NativeRecordsStateContextSelector, 0, len(rawSelectors))
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
		if !nativeRecordsTypePattern.MatchString(name) {
			return nil, configError("%s.type is invalid", entryPath)
		}
		if allowedTypes != nil {
			if _, exists := allowed[name]; !exists {
				return nil, configError("%s.type is not in tools.native.records.allowed_types", entryPath)
			}
		}
		filter := map[string]any{}
		if raw, exists := fields["filter"]; exists && strings.TrimSpace(string(raw)) != "null" {
			filter, err = decodeRecordsFilter(raw, entryPath+".filter")
			if err != nil {
				return nil, err
			}
		}
		var excludeAny []map[string]any
		if raw, exists := fields["exclude_any"]; exists && strings.TrimSpace(string(raw)) != "null" {
			var clauses []json.RawMessage
			if err := json.Unmarshal(raw, &clauses); err != nil || len(clauses) > int(maxFilterFields) {
				return nil, configError("%s.exclude_any must be an array within tools.native.records filter limits", entryPath)
			}
			excludeAny = make([]map[string]any, 0, len(clauses))
			for clauseIndex, clause := range clauses {
				decoded, err := decodeRecordsFilter(clause, fmt.Sprintf("%s.exclude_any[%d]", entryPath, clauseIndex))
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
				if !nativeRecordsFilterKeyPattern.MatchString(part) {
					invalidKey = true
					break
				}
			}
			if invalidKey {
				break
			}
		}
		if int64(len(filterKeys)) > maxFilterFields || invalidKey {
			return nil, configError("%s.filter/exclude_any exceeds tools.native.records filter limits", entryPath)
		}
		groupBy := ""
		if raw, exists := fields["group_by"]; exists && strings.TrimSpace(string(raw)) != "null" {
			if json.Unmarshal(raw, &groupBy) != nil {
				return nil, configError("%s.group_by must be a field path string", entryPath)
			}
			parts := strings.Split(groupBy, ".")
			if int64(len(parts)) > maxFilterDepth {
				return nil, configError("%s.group_by exceeds tools.native.records filter limits", entryPath)
			}
			for _, part := range parts {
				if !nativeRecordsGroupByPartPattern.MatchString(part) || part == "__proto__" || part == "prototype" || part == "constructor" {
					return nil, configError("%s.group_by must be a dot-separated field path of identifiers", entryPath)
				}
			}
			if _, constrained := filter[groupBy]; constrained {
				return nil, configError("%s.group_by must not also be a filter key", entryPath)
			}
		}
		limit := min(defaultNativeRecordsStateLimit, maxLimit)
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
		selectors = append(selectors, NativeRecordsStateContextSelector{Type: name, Filter: filter, ExcludeAny: excludeAny, Limit: limit, GroupBy: groupBy})
	}
	return selectors, nil
}

func decodeRecordsFilter(raw json.RawMessage, section string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var filter map[string]any
	if err := decoder.Decode(&filter); err != nil || filter == nil {
		return nil, configError("%s must be an object", section)
	}
	return filter, nil
}
