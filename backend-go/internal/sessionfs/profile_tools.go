package sessionfs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ProfileToolSettings is the optional tools block of a path profile
// (paths.profiles.<name>.tools). The session registers every tool; this block
// chooses which of them the profile's own model is shown. Go only.
type ProfileToolSettings struct {
	// Restricted is true when the profile has a tools block. Without one the model
	// sees every registered tool, exactly as before.
	Restricted bool
	// HasAllow is true when the block has an allow list. Without one, everything the
	// session registers is shown except what Deny names.
	HasAllow bool
	// Allow lists what the model sees: a native tool name ("read"), one MCP tool
	// ("mcp:<server>:<tool>") or every tool of one MCP server ("mcp:<server>:*").
	// An empty list with HasAllow hides every tool.
	Allow []string
	// Deny lists what is taken away, in the same entry form. It wins over Allow, and
	// it also limits the profile's sub-agents: a denied tool is in no child's catalog.
	Deny []string
	// Lazy lists the tools the model sees by name and summary only, through load_tool: it
	// describes a tool (full definition) and runs it; a call that names a lazy tool is refused.
	// Same entry forms as Allow and Deny. A lazy entry counts as allowed, and so does
	// load_tool while Lazy is non-empty, so neither needs an Allow entry; Deny still wins.
	// Nil when the block has no lazy list.
	Lazy []string
	// LazySummaryBytes bounds the first line of a lazy tool's description that the
	// model sees in place of the full description.
	LazySummaryBytes int64
	// LazyLoadMax bounds how many lazy tools one load_tool describe call (names) may match.
	LazyLoadMax int64
}

// profileToolsKeys are the keys paths.profiles.<name>.tools accepts.
var profileToolsKeys = []string{"allow", "deny", "lazy", "lazy_summary_bytes", "lazy_load_max"}

// Bounds of tools.lazy_summary_bytes; the default keeps one summary line short enough
// that a long list of lazy tools stays small in the model's context.
const (
	defaultLazySummaryBytes = 256
	minLazySummaryBytes     = 1
	maxLazySummaryBytes     = 4096
)

// Bounds of tools.lazy_load_max, the most lazy tools one load_tool describe call may match.
const (
	defaultLazyLoadMax = 16
	minLazyLoadMax     = 1
	maxLazyLoadMax     = 100
)

// LoadToolName is the native tool that lists, describes and runs the lazy tools. It
// is enabled only by tools.native.load_tool, like every native tool; tools.lazy needs it
// (checkLazyNeedsLoadTool).
const LoadToolName = "load_tool"

// ProfileToolMCPPrefix starts every MCP entry of the allow and deny lists.
const ProfileToolMCPPrefix = "mcp:"

// normalizeProfileTools validates one profile's tools block. The shape is checked
// here; whether each entry names a registered tool is checked when the catalog is
// built, because that needs the MCP servers' tool lists.
func normalizeProfileTools(raw json.RawMessage, profilePath string) (ProfileToolSettings, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ProfileToolSettings{}, configError("%s.tools must be an object", profilePath)
	}
	for key := range fields {
		if !containsString(profileToolsKeys, key) {
			return ProfileToolSettings{}, unsupportedToolKey(profilePath+".tools", key, profileToolsKeys, "")
		}
	}
	allowRaw, hasAllow := fields["allow"]
	denyRaw, hasDeny := fields["deny"]
	if !hasAllow && !hasDeny {
		return ProfileToolSettings{}, configError("%s.tools needs allow or deny; use an empty allow array to hide every tool", profilePath)
	}
	settings := ProfileToolSettings{Restricted: true, HasAllow: hasAllow}
	var err error
	if settings.LazySummaryBytes, err = configuredInteger(fields, "lazy_summary_bytes", defaultLazySummaryBytes, minLazySummaryBytes, maxLazySummaryBytes, profilePath, "tools"); err != nil {
		return ProfileToolSettings{}, err
	}
	if settings.LazyLoadMax, err = configuredInteger(fields, "lazy_load_max", defaultLazyLoadMax, minLazyLoadMax, maxLazyLoadMax, profilePath, "tools"); err != nil {
		return ProfileToolSettings{}, err
	}
	if lazyRaw, hasLazy := fields["lazy"]; hasLazy {
		if settings.Lazy, err = normalizeProfileToolList(lazyRaw, "lazy", profilePath); err != nil {
			return ProfileToolSettings{}, err
		}
		// load_tool is what shows a lazy tool, so it cannot itself wait to be loaded.
		for index, entry := range settings.Lazy {
			if entry == LoadToolName {
				return ProfileToolSettings{}, configError("%s.tools.lazy[%d] %q cannot be lazy: it is the tool that loads the others", profilePath, index, LoadToolName)
			}
		}
	}
	if hasAllow {
		if settings.Allow, err = normalizeProfileToolList(allowRaw, "allow", profilePath); err != nil {
			return ProfileToolSettings{}, err
		}
	}
	if hasDeny {
		if settings.Deny, err = normalizeProfileToolList(denyRaw, "deny", profilePath); err != nil {
			return ProfileToolSettings{}, err
		}
	}
	return settings, nil
}

// checkLazyNeedsLoadTool refuses a profile whose tools.lazy is set while tools.native.load_tool is
// not enabled. Nothing else can show a lazy tool, so without load_tool the lazy tools could never be
// loaded and the profile would be silently unusable. A profile with no lazy tools needs nothing.
func checkLazyNeedsLoadTool(settingPath, profile string, settings ProfileToolSettings, nativeLoadTool *NativeLoadToolSettings) error {
	if len(settings.Lazy) == 0 || nativeLoadTool != nil {
		return nil
	}
	return configError("%s: paths.profiles.%s.tools.lazy lists %d tool(s), but tools.native.%s is not enabled: add \"%s\": {} under tools.native, or remove tools.lazy", settingPath, profile, len(settings.Lazy), LoadToolName, LoadToolName)
}

// normalizeProfileToolList validates one tools.allow or tools.deny array.
func normalizeProfileToolList(raw json.RawMessage, name, profilePath string) ([]string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, configError("%s.tools.%s must be an array of tool names", profilePath, name)
	}
	entries := make([]string, 0, len(items))
	for index, item := range items {
		// The entry is kept as written, not trimmed: the schema pattern refuses
		// whitespace, so the loader must refuse it too (checkProfileToolEntry).
		var entry string
		if err := json.Unmarshal(item, &entry); err != nil || strings.TrimSpace(entry) == "" {
			return nil, configError("%s.tools.%s[%d] must be a non-empty string", profilePath, name, index)
		}
		if err := checkProfileToolEntry(entry); err != nil {
			return nil, configError("%s.tools.%s[%d] %v", profilePath, name, index, err)
		}
		if containsString(entries, entry) {
			return nil, configError("%s.tools.%s[%d] repeats %q", profilePath, name, index, entry)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// CheckToolEntry checks one tools.allow entry: a native tool name, mcp:<server>:<tool> or
// mcp:<server>:*. The error text continues "<entry> ...".
func CheckToolEntry(entry string) error {
	return checkProfileToolEntry(entry)
}

// checkProfileToolEntry accepts a native tool name, mcp:<server>:<tool> or
// mcp:<server>:*. The error text continues the sentence "allow[i] ..." or "deny[i] ...".
func checkProfileToolEntry(entry string) error {
	if strings.IndexFunc(entry, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%q must not contain whitespace", entry)
	}
	if !strings.HasPrefix(entry, ProfileToolMCPPrefix) {
		if hint, removed := removedNativeTools[entry]; removed {
			return fmt.Errorf("%q was removed; %s", entry, removedToolListHint(hint))
		}
		if containsString(nativeToolNames, entry) || containsString(extensionToolNames, entry) {
			return nil
		}
		names := append(append([]string(nil), nativeToolNames...), extensionToolNames...)
		sort.Strings(names)
		return fmt.Errorf("%q is not a native tool; use one of %s, or mcp:<server>:<tool>", entry, strings.Join(names, ", "))
	}
	parts := strings.SplitN(entry, ":", 3)
	if len(parts) != 3 || !mcpServerIDPattern.MatchString(parts[1]) || parts[2] == "" || strings.HasSuffix(parts[2], ":*") {
		return fmt.Errorf("%q must be mcp:<server>:<tool> or mcp:<server>:*", entry)
	}
	return nil
}
