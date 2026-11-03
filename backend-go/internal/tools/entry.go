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

// ProfileShows reports whether the profile's model sees the tool: an allow list
// (when the block has one) must cover it, and no deny entry may. Every surface that
// hides or exposes a tool by profile calls it, so they agree.
func ProfileShows(settings sessionfs.ProfileToolSettings, definition Definition) bool {
	if AnyEntryMatches(settings.Deny, definition) {
		return false
	}
	return !settings.HasAllow || AnyEntryMatches(settings.Allow, definition)
}
