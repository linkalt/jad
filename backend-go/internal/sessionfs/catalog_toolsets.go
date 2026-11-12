package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	return validateLayoutToolset(layout, roots, path, toolset)
}

// validateLayoutToolset applies the toolset rules to a decoded toolset: it is composed into a
// scratch session, whose settings must load. An own toolset (phase 3c.3) is judged composed with
// its base, the way the profile's load applies it.
func validateLayoutToolset(layout Layout, roots []Root, path string, toolset layoutToolsetFile) error {
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
	// A workspace toolset reads its relative paths from W/.jad, as a session of the workspace
	// does; a global one has them made absolute from G by applyToolset.
	base := globalRoot
	if layout.Workspace != "" {
		base = filepath.Join(layout.Workspace, ".jad")
	}
	location := Location{SessionID: "toolset-check", Root: Root{GlobalJadRoot: base, Layout: &Layout{State: scratch}}}
	_, err = loadSessionSettingsFrom(location, setting, path, base, "", "")
	return err
}

// toolsetUsers lists the profiles whose toolset is id: they stop resolving when the file at path
// is removed. A profile file that cannot be read is not counted. A profile one of whose own
// toolsets is based on the file counts too (phase 3c.3, decider 3c C4), in this workspace only.
func toolsetUsers(layout Layout, id, path string) ([]string, error) {
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	var users []string
	// One memo for every profile: the shared folders are scanned once.
	scans := NewCatalogScans()
	for _, entry := range profiles.Entries() {
		if path != "" {
			own, err := layout.profileToolsets(scans, entry.ID())
			if err != nil {
				return nil, configError("%v", err)
			}
			if base := own.basedOn(path); base != "" {
				users = append(users, entry.ID()+" (its own toolset "+base+" is based on it)")
				continue
			}
		}
		var profile layoutProfileFile
		if err := readLayoutFile(entry.Path, &profile); err != nil {
			continue
		}
		// A scope-prefixed reference counts too (it may name this file; refusing is the safe
		// side); a profile: reference names an own toolset, never this file.
		if slices.ContainsFunc(profile.Toolset, func(ref string) bool {
			return ref == id || !ownReference(ref) && strings.HasSuffix(ref, ":"+id)
		}) {
			users = append(users, entry.ID())
		}
	}
	return users, nil
}
