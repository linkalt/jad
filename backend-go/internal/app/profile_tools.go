package app

import (
	"fmt"
	"sort"
	"strings"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

// applyProfileTools narrows the registered tools to what the selected profile's model
// may see. It runs on the merged catalog, after the sub-agent tool built the catalogs
// of its agents, so a tool the profile hides stays available to a child that lists it.
// A profile without a tools block gets the catalog back unchanged.
func applyProfileTools(config sessionfs.SessionConfig, registered *tools.Catalog) (*tools.Catalog, []tools.Definition, error) {
	settings := config.ProfileTools
	if !settings.Restricted {
		return registered, nil, nil
	}
	definitions := registered.Definitions()
	if err := checkProfileToolEntries(config, settings.Allow, definitions); err != nil {
		return nil, nil, err
	}
	exposed, hidden, err := registered.Filter(func(definition tools.Definition) bool {
		return profileToolAllowed(settings.Allow, definition)
	})
	if err != nil {
		return nil, nil, err
	}
	return exposed, hidden, nil
}

// profileToolAllowed reports whether one allow-list entry covers the definition.
func profileToolAllowed(allow []string, definition tools.Definition) bool {
	for _, entry := range allow {
		if profileToolMatches(entry, definition) {
			return true
		}
	}
	return false
}

func profileToolMatches(entry string, definition tools.Definition) bool {
	if !strings.HasPrefix(entry, sessionfs.ProfileToolMCPPrefix) {
		return definition.Source == "native" && definition.Name == entry
	}
	if definition.Source != "mcp" {
		return false
	}
	if prefix, found := strings.CutSuffix(entry, ":*"); found {
		return strings.HasPrefix(definition.ID, prefix+":")
	}
	return definition.ID == entry
}

// checkProfileToolEntries refuses an entry that names nothing registered, so a typo
// shows up as a configuration error and not as a tool that is quietly missing.
func checkProfileToolEntries(config sessionfs.SessionConfig, allow []string, registered []tools.Definition) error {
	for _, entry := range allow {
		if profileToolEntryRegistered(config, entry, registered) {
			continue
		}
		return &sessionfs.ConfigError{Message: fmt.Sprintf("%s: paths.profiles.%s.tools.allow lists %q, which is not a registered tool (registered: %s)",
			config.SettingPath, config.Profile, entry, registeredToolList(registered))}
	}
	return nil
}

func profileToolEntryRegistered(config sessionfs.SessionConfig, entry string, registered []tools.Definition) bool {
	if strings.HasSuffix(entry, ":*") && strings.HasPrefix(entry, sessionfs.ProfileToolMCPPrefix) {
		// A whole server is registered when the session lists it as enabled, even if
		// it exposes no tool right now.
		server := strings.TrimSuffix(strings.TrimPrefix(entry, sessionfs.ProfileToolMCPPrefix), ":*")
		for _, reference := range config.MCPServers {
			if reference.ID == server && reference.Enabled {
				return true
			}
		}
		return false
	}
	for _, definition := range registered {
		if profileToolMatches(entry, definition) {
			return true
		}
	}
	return false
}

// registeredToolList names the tools a profile may list: native tools by name, MCP
// tools by id.
func registeredToolList(registered []tools.Definition) string {
	names := make([]string, 0, len(registered))
	for _, definition := range registered {
		if definition.Source == "mcp" {
			names = append(names, definition.ID)
		} else {
			names = append(names, definition.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
