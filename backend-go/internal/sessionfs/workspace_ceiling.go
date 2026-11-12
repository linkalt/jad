package sessionfs

import (
	"encoding/json"
	"slices"
	"strings"
)

// The ordered ceiling keys of the workspace layout (design Q7): a profile file may tighten
// them, never loosen them, against the layers below it (the global and workspace defaults and
// the session). A profile that loosens one is left out with a warning when it is not selected;
// selected, its load fails. Keys without an order (hooks, env_file, cwd, tool_storage_dir, MCP
// and exec command definitions) are not checked here; v1 layouts are never checked.

// ceilingCaps are the numeric caps, as paths into the merged settings: a set cap (above 0)
// below the profile can only go down.
var ceilingCaps = [][]string{
	{"max_run_tokens"}, {"max_run_cost_usd"}, {"max_run_duration_ms"},
	{"budget", "max_session_tokens"}, {"budget", "max_session_cost_usd"},
	{"budget", "max_daily_tokens"}, {"budget", "max_daily_cost_usd"},
}

// checkProfileCeilings refuses a profile of a workspace-layout session whose settings loosen an
// ordered key the lower layers set.
func checkProfileCeilings(layout Layout, session layoutSessionFile, profilePath string, profile map[string]json.RawMessage, scans *CatalogScans) error {
	if layout.Workspace == "" || len(profile) == 0 {
		return nil
	}
	sessionLayer := mergeSettings(nil, session.Settings)
	if session.CWD != "" {
		sessionLayer["cwd"] = mustJSON(session.CWD)
	}
	lower := mergeSettings(layout.Defaults, sessionLayer)
	full := mergeSettings(lower, profile)
	for _, path := range ceilingCaps {
		below, set := settingNumber(lower, path)
		if !set || below <= 0 {
			continue
		}
		if value, ok := settingNumber(full, path); !ok || value <= 0 || value > below {
			return configError("%s: settings.%s may not be raised or removed by a profile in this workspace: the defaults or the session set it to %v; change it there", profilePath, strings.Join(path, "."), below)
		}
	}
	if _, set := profile["sandbox"]; set {
		return checkSandboxCeiling(layout, sessionLayer, full, profilePath, profile["sandbox"], scans)
	}
	return nil
}

// settingNumber reads the number at path; false when it is absent, null or not a number.
func settingNumber(settings map[string]json.RawMessage, path []string) (float64, bool) {
	raw, ok := settings[path[0]]
	for _, key := range path[1:] {
		var fields map[string]json.RawMessage
		if !ok || json.Unmarshal(raw, &fields) != nil || fields == nil {
			return 0, false
		}
		raw, ok = fields[key]
	}
	var value float64
	if !ok || json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

// checkSandboxCeiling compares the sandbox the lower layers give with the one the profile's
// layer makes of it. When the lower one is enabled, the profile may not turn it off, change
// its program, cover fewer tools, hide fewer paths, or widen what it writes, reads or reaches
// on the network. Paths are compared as written: an equal path spelled differently is refused.
// A composition that fails is left to the load, which reports it.
func checkSandboxCeiling(layout Layout, sessionLayer, full map[string]json.RawMessage, profilePath string, profileSandbox json.RawMessage, scans *CatalogScans) error {
	var cwd string
	if json.Unmarshal(full["cwd"], &cwd) == nil {
		cwd = expandHome(strings.TrimSpace(cwd))
	}
	templates := sandboxTemplates{dir: layout.SandboxTemplateDir(cwd), scans: scans}
	lowerLayers := []json.RawMessage{layout.Defaults["sandbox"], sessionLayer["sandbox"]}
	wheres := []string{"defaults", "session settings", profilePath + ": settings"}
	lowerRaw, absent, err := composeSandboxLayers(lowerLayers, wheres[:2], templates)
	if err != nil || absent {
		return nil
	}
	below, enabled, err := parseSandbox(lowerRaw, profilePath, "sandbox")
	if err != nil || !enabled {
		return nil
	}
	fullRaw, absent, err := composeSandboxLayers(append(lowerLayers, profileSandbox), wheres, templates)
	if err != nil {
		return nil
	}
	refuse := func(what string) error {
		return configError("%s: settings.sandbox may not %s in this workspace: the defaults or the session enable a sandbox that a profile can only tighten; change it there", profilePath, what)
	}
	if absent {
		return refuse("be removed")
	}
	above, enabled, err := parseSandbox(fullRaw, profilePath, "sandbox")
	if err != nil {
		return nil
	}
	if !enabled {
		return refuse("turn the sandbox off")
	}
	if above.Backend != below.Backend || above.Executable != below.Executable || !slices.Equal(above.Command, below.Command) {
		return refuse("change backend, executable or command")
	}
	if missing := missingFrom(above.Tools, below.Tools); missing != "" {
		return refuse("stop covering tool " + missing)
	}
	if missing := missingFrom(above.HidePaths, below.HidePaths); missing != "" {
		return refuse("stop hiding " + missing)
	}
	if extra := missingFrom(below.WritablePaths, above.WritablePaths); extra != "" {
		return refuse("make " + extra + " writable")
	}
	if extra := missingFrom(below.PlanWritablePaths, above.PlanWritablePaths); extra != "" {
		return refuse("make " + extra + " writable in plan mode")
	}
	if !below.WholeFileSystemReadable() {
		if above.WholeFileSystemReadable() {
			return refuse("make the whole file system readable")
		}
		if extra := missingFrom(below.ReadPaths, above.ReadPaths); extra != "" {
			return refuse("make " + extra + " readable")
		}
	}
	if above.Network && !below.Network {
		return refuse("turn the network on")
	}
	// Without the network of its own, the allow list can only shrink (an empty list is none).
	if !below.Network {
		if extra := missingFrom(below.NetworkAllow, above.NetworkAllow); extra != "" {
			return refuse("allow the network host " + extra)
		}
	}
	return nil
}

// missingFrom is the first entry of want that have lacks, or "".
func missingFrom(have, want []string) string {
	for _, entry := range want {
		if !slices.Contains(have, entry) {
			return entry
		}
	}
	return ""
}
