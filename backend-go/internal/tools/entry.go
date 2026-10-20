package tools

import "strings"

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
