package sessionfs

import (
	"encoding/json"
	"os"
	"slices"

	"jad/backend-go/internal/catalog"
)

// validateToolsetFile applies what the loader applies to a toolset a run uses: the strict
// file shape, the MCP servers it names, and the same normalizers that read a session's
// tools, hooks and MCP list. The toolset is composed into a scratch session in an empty
// directory, so no tool storage or file of a real session is read or created.
func validateToolsetFile(layout Layout, roots []Root, path string, data []byte) error {
	var toolset layoutToolsetFile
	if err := parseLayoutFile(path, data, &toolset); err != nil {
		return err
	}
	if toolset.SchemaVersion != 0 && toolset.SchemaVersion != 1 {
		return configError("%s: unsupported schema_version %d", path, toolset.SchemaVersion)
	}
	scratch, err := os.MkdirTemp("", "jad-toolset-check-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	cwd, _ := json.Marshal(scratch)
	paths := json.RawMessage(`{"profiles":{"toolset-check":{}},"active_profile":"toolset-check"}`)
	setting := map[string]json.RawMessage{"schema_version": json.RawMessage("1"), "cwd": cwd, "paths": paths}
	compose := &layoutCompose{layout: layout}
	if err := compose.applyToolset(setting, toolset, path, nil); err != nil {
		return err
	}
	globalRoot := ""
	if len(roots) > 0 {
		globalRoot = roots[0].GlobalJadRoot
	}
	location := Location{SessionID: "toolset-check", Root: Root{GlobalJadRoot: globalRoot, Layout: &Layout{State: scratch}}}
	_, err = loadSessionSettingsFrom(location, setting, path, globalRoot, "", "")
	return err
}

// toolsetUsers lists the profiles whose toolset is id: they stop resolving when the file is
// removed. A profile file that cannot be read is not counted.
func toolsetUsers(layout Layout, id string) ([]string, error) {
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	var users []string
	for _, entry := range profiles.Entries() {
		var profile layoutProfileFile
		if err := readLayoutFile(entry.Path, &profile); err != nil {
			continue
		}
		if slices.Contains(profile.Toolset, id) {
			users = append(users, entry.ID())
		}
	}
	return users, nil
}
