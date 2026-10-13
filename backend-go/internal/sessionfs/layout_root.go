package sessionfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// layoutCatalogDefaults are the catalog folders under the root setting's parent when
// `catalogs` does not name them.
var layoutCatalogDefaults = map[string]string{
	"profiles": ".jad/profiles",
	"models":   ".jad/models",
	"tools":    ".jad/tools",
	"mcp":      ".jad/mcp",
	"state":    ".jad/state",
}

// layoutFromSetting reads `catalogs`, `defaults` and `libraries.tool_definition_paths`
// of the root setting. Folders are resolved against the setting file's directory.
func layoutFromSetting(setting map[string]json.RawMessage, settingPath string) (*Layout, error) {
	folders := make(map[string]string, len(layoutCatalogDefaults))
	for key, value := range layoutCatalogDefaults {
		folders[key] = value
	}
	if raw, exists := setting["catalogs"]; exists {
		var configured map[string]json.RawMessage
		if err := json.Unmarshal(raw, &configured); err != nil || configured == nil {
			return nil, configError("%s: catalogs must be an object", settingPath)
		}
		for key, rawValue := range configured {
			if _, known := layoutCatalogDefaults[key]; !known {
				return nil, configError("%s: catalogs.%s is not a catalog (profiles, models, tools, mcp, state)", settingPath, key)
			}
			var folder string
			if err := json.Unmarshal(rawValue, &folder); err != nil || strings.TrimSpace(folder) == "" {
				return nil, configError("%s: catalogs.%s must be a non-empty string", settingPath, key)
			}
			folders[key] = strings.TrimSpace(folder)
		}
	}
	base := filepath.Dir(settingPath)
	layout := &Layout{
		Profiles: resolveFrom(base, folders["profiles"]),
		Models:   resolveFrom(base, folders["models"]),
		Tools:    resolveFrom(base, folders["tools"]),
		MCP:      resolveFrom(base, folders["mcp"]),
		State:    resolveFrom(base, folders["state"]),
		Defaults: map[string]json.RawMessage{},
	}
	if raw, exists := setting["defaults"]; exists {
		if err := json.Unmarshal(raw, &layout.Defaults); err != nil || layout.Defaults == nil {
			return nil, configError("%s: defaults must be an object", settingPath)
		}
		if err := checkLayoutSettings(layout.Defaults, settingPath+": defaults"); err != nil {
			return nil, err
		}
	}
	if raw, exists := setting["libraries"]; exists {
		var libraries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &libraries); err != nil || libraries == nil {
			return nil, configError("%s: libraries must be an object", settingPath)
		}
		if rawPaths, ok := libraries["tool_definition_paths"]; ok {
			if err := json.Unmarshal(rawPaths, &layout.ToolDefinitionPaths); err != nil {
				return nil, configError("%s: libraries.tool_definition_paths must be an array of strings", settingPath)
			}
		}
	}
	return layout, nil
}

// SessionFileID is the session id a root directory entry names: "<id>.json" for a regular
// file, with an id the session routes accept. Lock files and temporary files start with a
// dot or end differently, so they are never sessions.
func SessionFileID(entry os.DirEntry) (string, bool) {
	name, found := strings.CutSuffix(entry.Name(), sessionFileSuffix)
	if !found || !sessionIDPattern.MatchString(name) || name == "." || name == ".." {
		return "", false
	}
	if !entry.Type().IsRegular() {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return "", false
		}
	}
	return name, true
}

// checkUniqueSessionIDs refuses roots that hold a session of the same id. The state folder
// of a session is <state>/<session>/..., and every root of one root setting shares the
// state catalog, so two such sessions would write the same transcript.
func checkUniqueSessionIDs(roots []Root, settingPath string) error {
	owners := map[string]string{}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			id, ok := SessionFileID(entry)
			if !ok {
				continue
			}
			if other, taken := owners[id]; taken {
				return configError("%s: session %q is in session roots %q and %q; they share one state folder, so rename one of the sessions", settingPath, id, other, root.ID)
			}
			owners[id] = root.ID
		}
	}
	return nil
}

// UsesLayout reports whether the session is stored in the catalog layout: its root has
// a layout and holds <session id>.json.
func (l Location) UsesLayout() bool {
	if l.Root.Layout == nil {
		return false
	}
	info, err := os.Stat(l.File)
	return err == nil && info.Mode().IsRegular()
}

// SettingFile is the file that holds the session's own configuration and that its
// writers and lock use: <session id>.json.
func (l Location) SettingFile() string {
	return l.File
}

// StateDir is <state>/<session id>, where the session keeps its transcripts, and the base
// of every path a session setting names relative to "the session folder". Empty when the
// root has no layout.
func (l Location) StateDir() string {
	if l.Root.Layout == nil {
		return ""
	}
	return filepath.Join(l.Root.Layout.State, l.SessionID)
}

// HasSetting reports whether the directory is a session of either kind.
func (l Location) HasSetting() (bool, error) {
	info, err := os.Stat(l.SettingFile())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect session file %q: %w", l.File, err)
	}
	return info.Mode().IsRegular(), nil
}
