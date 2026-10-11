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
			if !entry.IsDir() {
				continue
			}
			info, err := os.Stat(filepath.Join(root.Directory, entry.Name(), layoutSessionFileName))
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if other, taken := owners[entry.Name()]; taken {
				return configError("%s: session %q is in session roots %q and %q; they share one state folder, so rename one of the sessions", settingPath, entry.Name(), other, root.ID)
			}
			owners[entry.Name()] = root.ID
		}
	}
	return nil
}

// UsesLayout reports whether the session is stored in the catalog layout: its root has
// a layout and its directory holds a session.json.
func (l Location) UsesLayout() bool {
	if l.Root.Layout == nil {
		return false
	}
	info, err := os.Stat(filepath.Join(l.Directory, layoutSessionFileName))
	return err == nil && info.Mode().IsRegular()
}

// SettingFile is the file that holds the session's own configuration and that its
// writers and lock use: session.json in the catalog layout, setting.json otherwise.
func (l Location) SettingFile() string {
	if l.UsesLayout() {
		return filepath.Join(l.Directory, layoutSessionFileName)
	}
	return filepath.Join(l.Directory, "setting.json")
}

// HasSetting reports whether the directory is a session of either kind.
func (l Location) HasSetting() (bool, error) {
	info, err := os.Stat(l.SettingFile())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect session setting in %q: %w", l.Directory, err)
	}
	return info.Mode().IsRegular(), nil
}
