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
// chooses which of them the profile's own model is shown. A sub-agent is not
// affected: it draws from everything the session registers. Go only.
type ProfileToolSettings struct {
	// Restricted is true when the profile has a tools block. Without one the model
	// sees every registered tool, exactly as before.
	Restricted bool
	// Allow lists what the model sees: a native tool name ("read"), one MCP tool
	// ("mcp:<server>:<tool>") or every tool of one MCP server ("mcp:<server>:*").
	// An empty list hides every tool.
	Allow []string
}

// profileToolsKeys are the keys paths.profiles.<name>.tools accepts.
var profileToolsKeys = []string{"allow"}

// ProfileToolMCPPrefix starts every MCP entry of the allow list.
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
	allowRaw, exists := fields["allow"]
	if !exists {
		return ProfileToolSettings{}, configError("%s.tools.allow is required; use an empty array to hide every tool", profilePath)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(allowRaw, &items); err != nil || items == nil {
		return ProfileToolSettings{}, configError("%s.tools.allow must be an array of tool names", profilePath)
	}
	allow := make([]string, 0, len(items))
	for index, item := range items {
		// The entry is kept as written, not trimmed: the schema pattern refuses
		// whitespace, so the loader must refuse it too (checkProfileToolEntry).
		var entry string
		if err := json.Unmarshal(item, &entry); err != nil || strings.TrimSpace(entry) == "" {
			return ProfileToolSettings{}, configError("%s.tools.allow[%d] must be a non-empty string", profilePath, index)
		}
		if err := checkProfileToolEntry(entry); err != nil {
			return ProfileToolSettings{}, configError("%s.tools.allow[%d] %v", profilePath, index, err)
		}
		if containsString(allow, entry) {
			return ProfileToolSettings{}, configError("%s.tools.allow[%d] repeats %q", profilePath, index, entry)
		}
		allow = append(allow, entry)
	}
	return ProfileToolSettings{Restricted: true, Allow: allow}, nil
}

// checkProfileToolEntry accepts a native tool name, mcp:<server>:<tool> or
// mcp:<server>:*. The error text continues the sentence "allow[i] ...".
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
