package sessionfs

import (
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
)

const (
	defaultMaxToolRounds    int64 = 8
	maxConfiguredToolRounds int64 = 50_000
	// defaultMaxParallelToolCalls and maxConfiguredParallelToolCalls mirror the
	// TypeScript DEFAULT_MAX_PARALLEL_TOOL_CALLS and
	// MAX_CONFIGURED_PARALLEL_TOOL_CALLS.
	defaultMaxParallelToolCalls    int64 = 8
	maxConfiguredParallelToolCalls int64 = 64
	defaultNativeReadMaxBytes      int64 = 64 * 1024
	maxNativeToolBytes             int64 = 10 * 1024 * 1024
	defaultNativeReadMaxFile       int64 = 64 * 1024 * 1024
	maxNativeReadMaxFile           int64 = 1024 * 1024 * 1024
	defaultNativeReadLimit         int64 = 250
	maxSafeInteger                 int64 = 9_007_199_254_740_991
)

// NativeReadSettings are the normalized safety limits for the initial native
// read tool. Path rules are shared with session file listing behavior.
type NativeReadSettings struct {
	Policy       NativePathPolicy
	MaxBytes     int64
	MaxFileBytes int64
	DefaultLimit int64
}

func normalizeMaxToolRounds(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultMaxToolRounds, nil
	}
	value, ok := configSafeInteger(raw)
	if !ok || value < 0 || value > maxConfiguredToolRounds {
		return 0, configError("%s: max_tool_rounds must be a safe integer between 0 and %d", path, maxConfiguredToolRounds)
	}
	return value, nil
}

// normalizeMaxParallelToolCalls validates the most tool calls of one provider
// response that may run at the same time.
func normalizeMaxParallelToolCalls(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultMaxParallelToolCalls, nil
	}
	value, ok := configSafeInteger(raw)
	if !ok || value < 1 || value > maxConfiguredParallelToolCalls {
		return 0, configError("%s: max_parallel_tool_calls must be a safe integer between 1 and %d", path, maxConfiguredParallelToolCalls)
	}
	return value, nil
}

func normalizeToolDefinitionPaths(raw json.RawMessage, globalJadRoot, path string) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, configError("%s: tool_definition_paths must be an array", path)
	}
	paths := make([]string, 0, len(values))
	for index, rawValue := range values {
		var configured string
		if err := json.Unmarshal(rawValue, &configured); err != nil || strings.TrimSpace(configured) == "" {
			return nil, configError("%s: tool_definition_paths[%d] must be a non-empty string", path, index)
		}
		configured = strings.TrimSpace(configured)
		resolved := configured
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(globalJadRoot, resolved)
		}
		absolute, err := filepath.Abs(resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve tool definition path %q: %w", configured, err)
		}
		paths = append(paths, filepath.Clean(absolute))
	}
	return paths, nil
}

func normalizeNativeReadSettings(setting map[string]json.RawMessage, globalJadRoot, path string) (*NativeReadSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	tools, err := decodeConfigObject(toolsRaw, path+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := tools["native"]
	if !exists {
		return nil, nil
	}
	native, err := decodeConfigObject(nativeRaw, path+": tools.native")
	if err != nil {
		return nil, err
	}
	readRaw, exists := native["read"]
	if !exists {
		return nil, nil
	}
	read, err := decodeConfigObject(readRaw, path+": tools.native.read")
	if err != nil {
		return nil, err
	}
	for key := range read {
		if key != "max_bytes" && key != "max_file_bytes" && key != "default_limit" && key != "paths" {
			return nil, configError("%s: tools.native.read.%s is not supported", path, key)
		}
	}
	policy, err := normalizeNativeReadPolicy(setting, globalJadRoot, path)
	if err != nil {
		return nil, err
	}
	maxBytes, err := configuredInteger(read, "max_bytes", defaultNativeReadMaxBytes, 1, maxNativeToolBytes, path, "tools.native.read")
	if err != nil {
		return nil, err
	}
	maxFileBytes, err := configuredInteger(read, "max_file_bytes", defaultNativeReadMaxFile, 1, maxNativeReadMaxFile, path, "tools.native.read")
	if err != nil {
		return nil, err
	}
	defaultLimit, err := configuredInteger(read, "default_limit", defaultNativeReadLimit, 1, maxSafeInteger, path, "tools.native.read")
	if err != nil {
		return nil, err
	}
	if uint64(defaultLimit) > uint64(^uint(0)>>1) {
		return nil, configError("%s: tools.native.read.default_limit must be a positive safe integer", path)
	}
	return &NativeReadSettings{Policy: policy, MaxBytes: maxBytes, MaxFileBytes: maxFileBytes, DefaultLimit: defaultLimit}, nil
}

func configuredInteger(fields map[string]json.RawMessage, name string, fallback, minimum, maximum int64, path, section string) (int64, error) {
	raw, exists := fields[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return fallback, nil
	}
	value, ok := configSafeInteger(raw)
	if !ok || value < minimum || value > maximum {
		return 0, configError("%s: %s.%s must be a safe integer between %d and %d", path, section, name, minimum, maximum)
	}
	return value, nil
}

func configSafeInteger(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	value := rational.Num().Int64()
	if value > maxSafeInteger || value < -maxSafeInteger {
		return 0, false
	}
	return value, true
}
