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
}

// profileToolsKeys are the keys paths.profiles.<name>.tools accepts.
var profileToolsKeys = []string{"allow", "deny"}

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

// checkProfileToolEntry accepts a native tool name, mcp:<server>:<tool> or
// mcp:<server>:*. The error text continues the sentence "allow[i] ..." or "deny[i] ...".
func checkProfileToolEntry(entry string) error {
	if strings.IndexFunc(entry, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%q must not contain whitespace", entry)
	}
	if !strings.HasPrefix(entry, ProfileToolMCPPrefix) {
		if containsString(nativeToolNames, entry) {
			return nil
		}
		names := append([]string(nil), nativeToolNames...)
		sort.Strings(names)
		return fmt.Errorf("%q is not a native tool; use one of %s, or mcp:<server>:<tool>", entry, strings.Join(names, ", "))
	}
	parts := strings.SplitN(entry, ":", 3)
	if len(parts) != 3 || !mcpServerIDPattern.MatchString(parts[1]) || parts[2] == "" || strings.HasSuffix(parts[2], ":*") {
		return fmt.Errorf("%q must be mcp:<server>:<tool> or mcp:<server>:*", entry)
	}
	return nil
}
