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
// A profile without a tools block gets the catalog back unchanged (its load_tool lazy list, if any,
// is still checked).
func applyProfileTools(config sessionfs.SessionConfig, registered *tools.Catalog) (*tools.Catalog, []tools.Definition, error) {
	settings := config.ProfileTools
	if !settings.Restricted {
		// No tools block, but load_tool's lazy list (a toolset's) still has to name tools that
		// exist: the same checks, and the catalog comes back unchanged.
		if len(settings.Lazy) > 0 {
			definitions := registered.Definitions()
			if err := checkLazyEntriesVisible(config, settings, definitions); err != nil {
				return nil, nil, err
			}
			if err := checkLazyLoadToolShown(config, settings, definitions); err != nil {
				return nil, nil, err
			}
		}
		return registered, nil, nil
	}
	definitions := registered.Definitions()
	if err := checkProfileToolEntries(config, "allow", settings.Allow, definitions); err != nil {
		return nil, nil, err
	}
	if err := checkProfileToolEntries(config, "deny", settings.Deny, definitions); err != nil {
		return nil, nil, err
	}
	if err := checkLazyEntriesVisible(config, settings, definitions); err != nil {
		return nil, nil, err
	}
	if err := checkLazyLoadToolShown(config, settings, definitions); err != nil {
		return nil, nil, err
	}
	exposed, hidden, err := registered.Filter(func(definition tools.Definition) bool {
		return tools.ProfileShows(settings, definition)
	})
	if err != nil {
		return nil, nil, err
	}
	return exposed, hidden, nil
}

// openLazySet builds the profile's lazy set for one prepare, from the lazy list and summary bound
// of its load_tool config (tools.native.load_tool, in a toolset). It is nil when the profile has no lazy
// tools. The set lives in memory only (no state file, so no catalog layout is needed), and each
// prepare builds its own, so a profile_message target with its own lazy list never shares the
// caller's.
func openLazySet(config sessionfs.SessionConfig) *tools.LazySet {
	// The loader fills ProfileTools.Lazy from tools.native.load_tool only, so a list with no
	// load_tool config cannot come from a loaded session.
	if len(config.ProfileTools.Lazy) == 0 || config.NativeLoadTool == nil {
		return nil
	}
	shown := func(definition tools.Definition) bool { return tools.ProfileShows(config.ProfileTools, definition) }
	return tools.NewLazySet(config.ProfileTools.Lazy, int(config.NativeLoadTool.SummaryBytes), shown)
}

// checkLazyLoadToolShown refuses a profile whose load_tool config lists lazy tools
// (tools.native.load_tool.lazy) while its deny hides load_tool (allow cannot: a lazy list shows
// load_tool, see tools.ProfileShows). The lazy tools would then have no
// way to load, so the profile is refused rather than run with them unreachable. A profile with no lazy
// tools is not checked.
func checkLazyLoadToolShown(config sessionfs.SessionConfig, settings sessionfs.ProfileToolSettings, registered []tools.Definition) error {
	if len(settings.Lazy) == 0 {
		return nil
	}
	for _, definition := range registered {
		if definition.Name != sessionfs.LoadToolName || definition.Source != "native" || tools.ProfileShows(settings, definition) {
			continue
		}
		return &sessionfs.ConfigError{Message: fmt.Sprintf("%s: paths.profiles.%s.tools hides %q, but tools.native.load_tool.lazy lists %d tool(s) that only %q can load: remove %q from tools.deny, or remove tools.native.load_tool.lazy from its toolset",
			config.SettingPath, config.Profile, sessionfs.LoadToolName, len(settings.Lazy), sessionfs.LoadToolName, sessionfs.LoadToolName)}
	}
	return nil
}

// checkLazyEntriesVisible refuses a tools.native.load_tool.lazy entry that names no MCP or extension tool the profile
// shows: a lazy tool that is denied could never be run (a lazy entry needs no allow entry:
// tools.ProfileShows counts it as allowed), and a native tool is never lazy, so it would stay in
// the array. Either way the entry is a configuration error and not a tool that is quietly absent
// or quietly ignored.
func checkLazyEntriesVisible(config sessionfs.SessionConfig, settings sessionfs.ProfileToolSettings, registered []tools.Definition) error {
	for index, entry := range settings.Lazy {
		visible := false
		for _, definition := range registered {
			if tools.LazyEntryMatches(entry, definition) && tools.ProfileShows(settings, definition) {
				visible = true
				break
			}
		}
		if visible {
			continue
		}
		return &sessionfs.ConfigError{Message: fmt.Sprintf("%s: profile %s: tools.native.load_tool.lazy[%d] lists %q, which names no MCP or extension tool the profile shows (a denied tool cannot be lazy, and a native tool never is)",
			config.SettingPath, config.Profile, index, entry)}
	}
	return nil
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

// checkHookMatches refuses a pre_tool or post_tool hook whose match names no registered
// tool, which would otherwise never fire (a blocking guard silently absent). A match
// naming a disabled MCP server only warns: the server may be switched on again.
func checkHookMatches(config sessionfs.SessionConfig, registered []tools.Definition, warn func(string)) error {
	for event, list := range map[string][]sessionfs.ToolEventHook{"pre_tool": config.Hooks.PreTool, "post_tool": config.Hooks.PostTool} {
		for index, hook := range list {
			if hook.Match == "*" || profileToolEntryRegistered(config, hook.Match, registered) {
				continue
			}
			if hookMatchNamesDisabledServer(config, hook.Match) {
				if warn != nil {
					warn(fmt.Sprintf("%s: hooks.%s[%d].match %q names a disabled MCP server; the hook does not fire", config.SettingPath, event, index, hook.Match))
				}
				continue
			}
			return &sessionfs.ConfigError{Message: fmt.Sprintf("%s: hooks.%s[%d].match lists %q, which is not a registered tool (registered: %s)",
				config.SettingPath, event, index, hook.Match, registeredToolList(registered))}
		}
	}
	return nil
}

func hookMatchNamesDisabledServer(config sessionfs.SessionConfig, match string) bool {
	rest, ok := strings.CutPrefix(match, sessionfs.ProfileToolMCPPrefix)
	if !ok {
		return false
	}
	server, _, _ := strings.Cut(rest, ":")
	for _, reference := range config.MCPServers {
		if reference.ID == server && !reference.Enabled {
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
