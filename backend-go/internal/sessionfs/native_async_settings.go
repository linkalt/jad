package sessionfs

import (
	"encoding/json"
	"strings"

	"jad/backend-go/internal/asyncmgr"
)

const maxNativeAsyncValue int64 = 10_000_000

var nativeAsyncKeys = []string{
	"max_handles", "max_handles_total", "default_block_ms", "max_block_ms", "default_wait_ms", "max_wait_ms",
	"wait_max_signals", "list_default_limit", "list_max_limit", "summary_max_bytes", "status_summary_max_bytes",
	"response_max_bytes", "keep_finished", "retention_days", "unread_max", "unread_ttl_days", "stop_wait_ms",
}

// NativeAsyncSettings holds the normalized tools.native.async settings: the bounds of the
// async manager. Every field has a default, so an empty object is valid. The key's presence
// also enables the async tool.
type NativeAsyncSettings struct {
	Config asyncmgr.Config
}

func normalizeNativeAsyncSettings(setting map[string]json.RawMessage, settingPath string) (*NativeAsyncSettings, error) {
	toolsRaw, exists := setting["tools"]
	if !exists {
		return nil, nil
	}
	toolFields, err := decodeConfigObject(toolsRaw, settingPath+": tools")
	if err != nil {
		return nil, err
	}
	nativeRaw, exists := toolFields["native"]
	if !exists || strings.TrimSpace(string(nativeRaw)) == "null" {
		return nil, nil
	}
	nativeFields, err := decodeConfigObject(nativeRaw, settingPath+": tools.native")
	if err != nil {
		return nil, err
	}
	raw, exists := nativeFields["async"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.async"
	fields, err := decodeConfigObject(raw, settingPath+": "+section)
	if err != nil {
		return nil, err
	}
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeAsyncKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeAsyncKeys, "")
		}
	}
	config := asyncmgr.DefaultConfig()
	targets := map[string]*int64{
		"max_handles": &config.MaxHandles, "max_handles_total": &config.MaxHandlesTotal,
		"default_block_ms": &config.DefaultBlockMs, "max_block_ms": &config.MaxBlockMs,
		"default_wait_ms": &config.DefaultWaitMs, "max_wait_ms": &config.MaxWaitMs,
		"wait_max_signals": &config.WaitMaxSignals, "list_default_limit": &config.ListDefaultLimit,
		"list_max_limit": &config.ListMaxLimit, "summary_max_bytes": &config.SummaryMaxBytes,
		"status_summary_max_bytes": &config.StatusSummaryBytes, "response_max_bytes": &config.ResponseMaxBytes,
		"keep_finished": &config.KeepFinished, "retention_days": &config.RetentionDays,
		"unread_max": &config.UnreadMax, "unread_ttl_days": &config.UnreadTTLDays,
		"stop_wait_ms": &config.StopWaitMs,
	}
	for _, key := range nativeAsyncKeys {
		target := targets[key]
		minimum := int64(1)
		if key == "retention_days" || key == "unread_ttl_days" || key == "stop_wait_ms" {
			minimum = 0
		}
		if *target, err = configuredInteger(fields, key, *target, minimum, maxNativeAsyncValue, settingPath, section); err != nil {
			return nil, err
		}
	}
	if err := config.Validate(); err != nil {
		return nil, configError("%s: %s: %v", settingPath, section, err)
	}
	return &NativeAsyncSettings{Config: config}, nil
}
