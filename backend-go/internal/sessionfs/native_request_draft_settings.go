package sessionfs

import (
	"encoding/json"
)

const (
	defaultNativeRequestDraftTimeoutMS                   int64 = 300_000
	defaultNativeRequestDraftHeadlessAutoAcceptTimeoutMS int64 = 600_000
	defaultNativeRequestDraftMaxTimeoutMS                int64 = 1_800_000
	defaultNativeRequestDraftExtendMS                    int64 = 300_000
	minNativeRequestDraftExtendMS                        int64 = 1_000
	defaultNativeRequestDraftMaxDraftBytes               int64 = 32 * 1024
	minNativeRequestDraftMaxDraftBytes                   int64 = 1_024
	maxNativeRequestDraftMaxDraftBytes                   int64 = 1_048_576
	defaultNativeRequestDraftMaxDraftDepth               int64 = 8
	maxNativeRequestDraftMaxDraftDepth                   int64 = 32
	defaultNativeRequestDraftMaxTitleChars               int64 = 200
	minNativeRequestDraftMaxTitleChars                   int64 = 8
	maxNativeRequestDraftMaxTitleChars                   int64 = 1_000
)

// NativeRequestDraftSettings contains normalized limits for native.request_draft.
type NativeRequestDraftSettings struct {
	TimeoutMS                   int64
	HeadlessAutoAcceptTimeoutMS int64
	MaxTimeoutMS                int64
	ExtendMS                    int64
	MaxDraftBytes               int64
	MaxDraftDepth               int64
	MaxTitleChars               int64
}

func normalizeNativeRequestDraftSettings(setting map[string]json.RawMessage, settingPath string) (*NativeRequestDraftSettings, error) {
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
	raw, exists := nativeFields["request_draft"]
	if !exists {
		return nil, nil
	}
	fields, err := decodeConfigObject(raw, settingPath+": tools.native.request_draft")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "timeout_ms", "headless_auto_accept_timeout_ms", "max_timeout_ms", "extend_ms", "max_draft_bytes", "max_draft_depth", "max_title_chars":
		default:
			return nil, configError("%s: tools.native.request_draft.%s is not supported", settingPath, key)
		}
	}

	const settingName = "tools.native.request_draft"
	settings := &NativeRequestDraftSettings{}
	if settings.TimeoutMS, err = configuredInteger(fields, "timeout_ms", defaultNativeRequestDraftTimeoutMS, 1, defaultNativeRequestDraftMaxTimeoutMS, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.HeadlessAutoAcceptTimeoutMS, err = configuredInteger(fields, "headless_auto_accept_timeout_ms", defaultNativeRequestDraftHeadlessAutoAcceptTimeoutMS, 1, defaultNativeRequestDraftMaxTimeoutMS, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.MaxTimeoutMS, err = configuredInteger(fields, "max_timeout_ms", defaultNativeRequestDraftMaxTimeoutMS, 1, defaultNativeRequestDraftMaxTimeoutMS, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.TimeoutMS > settings.MaxTimeoutMS {
		return nil, configError("%s: tools.native.request_draft.timeout_ms must not exceed max_timeout_ms", settingPath)
	}
	if settings.ExtendMS, err = configuredInteger(fields, "extend_ms", defaultNativeRequestDraftExtendMS, minNativeRequestDraftExtendMS, defaultNativeRequestDraftMaxTimeoutMS, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.MaxDraftBytes, err = configuredInteger(fields, "max_draft_bytes", defaultNativeRequestDraftMaxDraftBytes, minNativeRequestDraftMaxDraftBytes, maxNativeRequestDraftMaxDraftBytes, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.MaxDraftDepth, err = configuredInteger(fields, "max_draft_depth", defaultNativeRequestDraftMaxDraftDepth, 1, maxNativeRequestDraftMaxDraftDepth, settingPath, settingName); err != nil {
		return nil, err
	}
	if settings.MaxTitleChars, err = configuredInteger(fields, "max_title_chars", defaultNativeRequestDraftMaxTitleChars, minNativeRequestDraftMaxTitleChars, maxNativeRequestDraftMaxTitleChars, settingPath, settingName); err != nil {
		return nil, err
	}
	return settings, nil
}
