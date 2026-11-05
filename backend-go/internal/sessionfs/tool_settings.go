package sessionfs

import (
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
)

const (
	// maxToolTimeoutMs is the ceiling of every tool timeout, block and wait setting: 6 hours.
	maxToolTimeoutMs        int64 = 21_600_000
	defaultMaxToolRounds    int64 = 8
	maxConfiguredToolRounds int64 = 50_000
	// defaultMaxParallelToolCalls and maxConfiguredParallelToolCalls mirror the
	// TypeScript DEFAULT_MAX_PARALLEL_TOOL_CALLS and
	// MAX_CONFIGURED_PARALLEL_TOOL_CALLS.
	defaultMaxParallelToolCalls    int64 = 8
	maxConfiguredParallelToolCalls int64 = 64
	maxNativeToolBytes             int64 = 10 * 1024 * 1024
	maxSafeInteger                 int64 = 9_007_199_254_740_991
)

func normalizeMaxToolRounds(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultMaxToolRounds, nil
	}
	value, ok := plainToolInteger(raw)
	if !ok || value < 0 || value > maxConfiguredToolRounds {
		return 0, configError("%s: max_tool_rounds must be a safe integer between 0 and %d", path, maxConfiguredToolRounds)
	}
	return value, nil
}

// plainToolInteger reads a tool limit written as a plain integer. It refuses exponent
// notation such as 1e3, which configSafeInteger accepts, so a tool limit is written the
// same way as the image store limits.
func plainToolInteger(raw json.RawMessage) (int64, bool) {
	if strings.ContainsAny(string(raw), "eE") {
		return 0, false
	}
	return configSafeInteger(raw)
}

// maxConfiguredRunTokens mirrors the TypeScript MAX_CONFIGURED_RUN_TOKENS.
const maxConfiguredRunTokens int64 = 1_000_000_000_000

// normalizeMaxRunTokens validates the most tokens one run may use. An absent key
// means no cap and is returned as 0; null and every other non-integer is an error.
func normalizeMaxRunTokens(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	value, ok := plainToolInteger(raw)
	if !ok || value < 1 || value > maxConfiguredRunTokens {
		return 0, configError("%s: max_run_tokens must be a safe integer between 1 and %d", path, maxConfiguredRunTokens)
	}
	return value, nil
}

// maxConfiguredRunDurationMS is a week: a cap for a run that should never be left alone
// that long, not a working limit.
const (
	minConfiguredRunDurationMS = 1_000
	maxConfiguredRunDurationMS = 604_800_000
)

// normalizeMaxRunDuration validates the most milliseconds one run may take. An absent key
// means no limit.
func normalizeMaxRunDuration(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	value, ok := plainToolInteger(raw)
	if !ok || value < minConfiguredRunDurationMS || value > maxConfiguredRunDurationMS {
		return 0, configError("%s: max_run_duration_ms must be a safe integer between %d and %d", path, minConfiguredRunDurationMS, maxConfiguredRunDurationMS)
	}
	return value, nil
}

// normalizeMaxParallelToolCalls validates the most tool calls of one provider
// response that may run at the same time.
func normalizeMaxParallelToolCalls(raw json.RawMessage, path string) (int64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaultMaxParallelToolCalls, nil
	}
	value, ok := plainToolInteger(raw)
	if !ok || value < 1 || value > maxConfiguredParallelToolCalls {
		return 0, configError("%s: max_parallel_tool_calls must be a safe integer between 1 and %d", path, maxConfiguredParallelToolCalls)
	}
	return value, nil
}

// ToolCallScheduling values; they mirror the TypeScript backend.
const (
	ToolCallSchedulingOrdered    = "ordered"
	ToolCallSchedulingConcurrent = "concurrent"
	defaultToolCallScheduling    = ToolCallSchedulingOrdered
)

// normalizeToolCallScheduling validates how the calls of one provider response
// are scheduled. Only an absent key means the default; null is an error.
func normalizeToolCallScheduling(raw json.RawMessage, path string) (string, error) {
	if len(raw) == 0 {
		return defaultToolCallScheduling, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (value != ToolCallSchedulingOrdered && value != ToolCallSchedulingConcurrent) {
		return "", configError("%s: tool_call_scheduling must be \"ordered\" or \"concurrent\"", path)
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
