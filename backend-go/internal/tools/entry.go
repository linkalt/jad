package tools

import (
	"strings"

	"jad/backend-go/internal/sessionfs"
)

const mcpEntryPrefix = "mcp:"

// EntryMatches reports whether one entry of a profile's tools.allow or tools.deny
// covers the definition. An entry is a native tool name ("read"), one MCP tool
// ("mcp:<server>:<tool>") or every tool of one MCP server ("mcp:<server>:*"). Server
// "docs" does not cover "docsx".
func EntryMatches(entry string, definition Definition) bool {
	if !strings.HasPrefix(entry, mcpEntryPrefix) {
		return (definition.Source == "native" || definition.Source == "extension") && definition.Name == entry
	}
	if definition.Source != "mcp" {
		return false
	}
	if prefix, found := strings.CutSuffix(entry, ":*"); found {
		return strings.HasPrefix(definition.ID, prefix+":")
	}
	return definition.ID == entry
}

// AnyEntryMatches reports whether at least one entry covers the definition.
func AnyEntryMatches(entries []string, definition Definition) bool {
	for _, entry := range entries {
		if EntryMatches(entry, definition) {
			return true
		}
	}
	return false
}

// ProfileShows reports whether the profile's model sees the tool: no deny entry may
// cover it, and an allow list (when the block has one) must cover it. A tools.lazy
// entry counts as allowed (see lazyAllows), so a lazy tool need not be repeated in
// allow; while the profile has lazy tools, load_tool counts as allowed too, since it is
// their only way to load. Deny still wins over both. Every surface that hides or
// exposes a tool by profile calls it, so they agree.
func ProfileShows(settings sessionfs.ProfileToolSettings, definition Definition) bool {
	if AnyEntryMatches(settings.Deny, definition) {
		return false
	}
	if !settings.HasAllow || AnyEntryMatches(settings.Allow, definition) || lazyAllows(settings, definition) {
		return true
	}
	return len(settings.Lazy) > 0 && definition.Source == "native" && definition.Name == sessionfs.LoadToolName
}

// lazyAllows reports whether a tools.lazy entry allows the definition. An entry that
// names one tool does. A whole-server entry ("mcp:<server>:*") does only while allow
// names nothing of that server: when allow lists some of the server's tools, allow
// decides which of them are shown and the wildcard only makes those lazy, so a lazy
// wildcard never widens an allow list (a profile written before lazy entries counted as
// allowed shows the same tools).
func lazyAllows(settings sessionfs.ProfileToolSettings, definition Definition) bool {
	for _, entry := range settings.Lazy {
		if !EntryMatches(entry, definition) {
			continue
		}
		server, wildcard := strings.CutSuffix(entry, ":*")
		if !wildcard || !strings.HasPrefix(entry, mcpEntryPrefix) || !allowNamesServer(settings.Allow, server) {
			return true
		}
	}
	return false
}

// allowNamesServer reports whether an allow entry names a tool of the MCP server whose
// entry prefix is server ("mcp:<server>"), or the whole server.
func allowNamesServer(allow []string, server string) bool {
	for _, entry := range allow {
		if strings.HasPrefix(entry, server+":") {
			return true
		}
	}
	return false
}
