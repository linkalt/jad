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
	// Lazy is the runtime view of the lazy list: the tools the model sees by name and
	// summary only, through load_tool. It is never read from the profile's JSON: the list is
	// owned by the load_tool tool (tools.native.load_tool.lazy, in a toolset), and the loader copies
	// it here (loadSessionSettingsFrom) so that ProfileShows, the lazy set, the tool_smith bridge
	// and the app checks keep one rule. A lazy entry counts as allowed, and so does load_tool
	// while Lazy is non-empty, so neither needs an Allow entry; Deny still wins. It is set for
	// a profile with no tools block too (Restricted false). Nil when there is no list.
	Lazy []string
}

// profileToolsKeys are the keys paths.profiles.<name>.tools accepts.
var profileToolsKeys = []string{"allow", "deny"}

// movedProfileToolsKeys (sorted) were keys of a profile's tools block that now belong to the load_tool
// tool (tools.native.load_tool, in a toolset). They are refused by name, with where they went.
var movedProfileToolsKeys = []string{"lazy", "lazy_load_max", "lazy_summary_bytes"}

// LoadToolName is the native tool that lists, describes and runs the lazy tools. It
// is enabled only by tools.native.load_tool, like every native tool, and that key also carries
// the lazy list it runs.
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
	for _, key := range movedProfileToolsKeys {
		if _, moved := fields[key]; moved {
			return ProfileToolSettings{}, configError("%s.tools.%s is not a key of a tools block any more: it belongs to the load_tool tool, so remove it here and set tools.native.load_tool.%s in a toolset file the profile names ({\"native\": {\"load_tool\": {\"%s\": ...}}}); a sub-agent uses the lazy list of the profile that owns it", profilePath, key, key, key)
		}
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
	if hasAllow {
		if settings.Allow, err = normalizeToolEntries(allowRaw, profilePath+".tools.allow", false); err != nil {
			return ProfileToolSettings{}, err
		}
	}
	if hasDeny {
		if settings.Deny, err = normalizeToolEntries(denyRaw, profilePath+".tools.deny", false); err != nil {
			return ProfileToolSettings{}, err
		}
	}
	return settings, nil
}

// normalizeToolEntries validates one array of tool entries: field names it in messages
// (<path>.tools.allow, <path>.tools.native.load_tool.lazy), and an error continues "field[i] ...".
// lazy applies the lazy-list rule (checkLazyEntry) on top of the entry form.
func normalizeToolEntries(raw json.RawMessage, field string, lazy bool) ([]string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, configError("%s must be an array of tool names", field)
	}
	entries := make([]string, 0, len(items))
	for index, item := range items {
		// The entry is kept as written, not trimmed: the schema pattern refuses
		// whitespace, so the loader must refuse it too (checkProfileToolEntry).
		var entry string
		if err := json.Unmarshal(item, &entry); err != nil || strings.TrimSpace(entry) == "" {
			return nil, configError("%s[%d] must be a non-empty string", field, index)
		}
		if err := checkProfileToolEntry(entry); err != nil {
			return nil, configError("%s[%d] %v", field, index, err)
		}
		if lazy {
			if err := checkLazyEntry(entry); err != nil {
				return nil, configError("%s[%d] %v", field, index, err)
			}
		}
		if containsString(entries, entry) {
			return nil, configError("%s[%d] repeats %q", field, index, entry)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// checkLazyEntry refuses a lazy entry that names a native tool: only MCP tools and extension
// tools can be lazy, since a native tool is always in the model's tools array. load_tool is a native
// tool and also the one that loads the others. The error text continues "lazy[i] ...".
func checkLazyEntry(entry string) error {
	switch {
	case entry == LoadToolName:
		return fmt.Errorf("%q cannot be lazy: it is the tool that loads the others", entry)
	case containsString(nativeToolNames, entry):
		return fmt.Errorf("%q is a native tool and cannot be lazy: tools.native.load_tool.lazy takes MCP tools (mcp:<server>:<tool> or mcp:<server>:*) and extension tools (%s)", entry, strings.Join(extensionToolNames, ", "))
	}
	return nil
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
