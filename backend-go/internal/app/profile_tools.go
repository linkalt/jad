package app

import (
	"fmt"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
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
	if err := checkProfileToolEntries(config, "allow", settings.Allow, definitions); err != nil {
		return nil, nil, err
	}
	if err := checkProfileToolEntries(config, "deny", settings.Deny, definitions); err != nil {
		return nil, nil, err
	}
	exposed, hidden, err := registered.Filter(func(definition tools.Definition) bool {
		return profileToolShown(settings, definition)
	})
	if err != nil {
		return nil, nil, err
	}
	return exposed, hidden, nil
}

// profileToolShown reports whether the profile's model sees the tool: an allow list
// (when the block has one) must cover it, and no deny entry may.
func profileToolShown(settings sessionfs.ProfileToolSettings, definition tools.Definition) bool {
	if tools.AnyEntryMatches(settings.Deny, definition) {
		return false
	}
	return !settings.HasAllow || tools.AnyEntryMatches(settings.Allow, definition)
}

// checkProfileToolEntries refuses an entry that names nothing registered, so a typo
// shows up as a configuration error and not as a tool that is quietly missing. list is
// "allow" or "deny".
func checkProfileToolEntries(config sessionfs.SessionConfig, list string, entries []string, registered []tools.Definition) error {
	for _, entry := range entries {
		if profileToolEntryRegistered(config, entry, registered) {
			continue
		}
		return &sessionfs.ConfigError{Message: fmt.Sprintf("%s: paths.profiles.%s.tools.%s lists %q, which is not a registered tool (registered: %s)",
			config.SettingPath, config.Profile, list, entry, registeredToolList(registered))}
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
		if tools.EntryMatches(entry, definition) {
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
