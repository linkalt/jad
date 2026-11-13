package sessionfs

import (
	"encoding/json"
)

// nativeLoadToolKeys are the keys tools.native.load_tool accepts.
var nativeLoadToolKeys = []string{"lazy", "lazy_summary_bytes", "lazy_load_max"}

// Bounds of tools.native.load_tool.lazy_summary_bytes; the default keeps one summary line short
// enough that a long list of lazy tools stays small in the model's context.
const (
	defaultLazySummaryBytes = 256
	minLazySummaryBytes     = 1
	maxLazySummaryBytes     = 4096
)

// Bounds of tools.native.load_tool.lazy_load_max, the most lazy tools one load_tool describe call
// may match and the most items one load_tool runs call may list.
const (
	defaultLazyLoadMax = 16
	minLazyLoadMax     = 1
	maxLazyLoadMax     = 100
)

// NativeLoadToolSettings is tools.native.load_tool: its presence enables the tool, and the tool
// owns the lazy list it runs. The profile does not own any of this; a profile reaches it through
// the toolset it names.
type NativeLoadToolSettings struct {
	// Lazy lists the tools the model sees by name and summary only, through load_tool: MCP tools
	// (mcp:<server>:<tool>, mcp:<server>:*) and extension tools (web_search, decide_systemone).
	// The loader reads it only to fill SessionConfig.ProfileTools.Lazy, the view that
	// tools.ProfileShows reads; nothing else reads this field. Nil when the key is absent.
	Lazy []string
	// SummaryBytes bounds the first line of a lazy tool's description that the model sees in
	// place of the full description.
	SummaryBytes int64
	// LoadMax bounds how many lazy tools one load_tool describe call (names) may match, and how
	// many items one load_tool runs call may list.
	LoadMax int64
}

// normalizeNativeLoadToolSettings reads tools.native.load_tool. The value must be an object; {}
// enables the tool with no lazy tools and the default bounds.
func normalizeNativeLoadToolSettings(setting map[string]json.RawMessage, settingPath string) (*NativeLoadToolSettings, error) {
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
	raw, exists := nativeFields["load_tool"]
	if !exists {
		return nil, nil
	}
	const section = "tools.native.load_tool"
	keys, err := orderedObjectKeys(raw)
	if err != nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	for _, key := range keys {
		if !containsString(nativeLoadToolKeys, key) {
			return nil, unsupportedToolKey(settingPath+": "+section, key, nativeLoadToolKeys, "")
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, configError("%s: %s must be an object", settingPath, section)
	}
	settings := &NativeLoadToolSettings{}
	if settings.SummaryBytes, err = configuredInteger(fields, "lazy_summary_bytes", defaultLazySummaryBytes, minLazySummaryBytes, maxLazySummaryBytes, settingPath, section); err != nil {
		return nil, err
	}
	if settings.LoadMax, err = configuredInteger(fields, "lazy_load_max", defaultLazyLoadMax, minLazyLoadMax, maxLazyLoadMax, settingPath, section); err != nil {
		return nil, err
	}
	if lazyRaw, hasLazy := fields["lazy"]; hasLazy {
		if settings.Lazy, err = normalizeToolEntries(lazyRaw, settingPath+": "+section+".lazy", true); err != nil {
			return nil, err
		}
	}
	return settings, nil
}
