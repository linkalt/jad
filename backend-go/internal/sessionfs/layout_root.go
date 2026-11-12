package sessionfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// layoutCatalogDefaults are the catalog folders under the root setting's parent when
// `catalogs` does not name them. They are written <project>/.jad/<catalog>, which is right
// when the setting sits in the project folder; for a setting inside a ".jad" folder (the
// usual place) catalogFolderDefaults gives the folder names without the ".jad/" prefix.
var layoutCatalogDefaults = map[string]string{
	"profiles": ".jad/profiles",
	"models":   ".jad/models",
	"tools":    ".jad/tools",
	"mcp":      ".jad/mcp",
	"remotes":  ".jad/remotes",
	"state":    ".jad/state",
	"sandbox":  ".jad/sandbox",
}

// sandboxCatalogCWD starts a catalogs.sandbox value that each session takes from its own cwd.
const sandboxCatalogCWD = "{cwd}"

// layoutFromSetting reads `catalogs`, `defaults` and `libraries.tool_definition_paths`
// of the root setting. Folders are resolved against the setting file's directory. It also
// returns the warnings of the default folders: see catalogFolderDefaults.
func layoutFromSetting(setting map[string]json.RawMessage, settingPath string) (*Layout, []string, error) {
	configured := map[string]string{}
	if raw, exists := setting["catalogs"]; exists {
		var named map[string]json.RawMessage
		if err := json.Unmarshal(raw, &named); err != nil || named == nil {
			return nil, nil, configError("%s: catalogs must be an object", settingPath)
		}
		for key, rawValue := range named {
			if _, known := layoutCatalogDefaults[key]; !known {
				return nil, nil, configError("%s: catalogs.%s is not a catalog (profiles, models, tools, mcp, remotes, state, sandbox)", settingPath, key)
			}
			var folder string
			if err := json.Unmarshal(rawValue, &folder); err != nil || strings.TrimSpace(folder) == "" {
				return nil, nil, configError("%s: catalogs.%s must be a non-empty string", settingPath, key)
			}
			configured[key] = strings.TrimSpace(folder)
		}
	}
	folders, warnings := catalogFolderDefaults(settingPath, configured)
	for key, folder := range configured {
		folders[key] = folder
	}
	base := filepath.Dir(settingPath)
	layout := &Layout{
		Profiles: resolveFrom(base, folders["profiles"]),
		Models:   resolveFrom(base, folders["models"]),
		Tools:    resolveFrom(base, folders["tools"]),
		MCP:      resolveFrom(base, folders["mcp"]),
		Remotes:  resolveFrom(base, folders["remotes"]),
		State:    resolveFrom(base, folders["state"]),
		Defaults: map[string]json.RawMessage{},

		RootSetting: filepath.Clean(settingPath),
	}
	if err := layout.setSandboxCatalog(folders["sandbox"], base, settingPath); err != nil {
		return nil, nil, err
	}
	if raw, exists := setting["defaults"]; exists {
		if err := json.Unmarshal(raw, &layout.Defaults); err != nil || layout.Defaults == nil {
			return nil, nil, configError("%s: defaults must be an object", settingPath)
		}
		if err := checkLayoutSettings(layout.Defaults, settingPath+": defaults"); err != nil {
			return nil, nil, err
		}
	}
	if raw, exists := setting["libraries"]; exists {
		var libraries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &libraries); err != nil || libraries == nil {
			return nil, nil, configError("%s: libraries must be an object", settingPath)
		}
		if rawPaths, ok := libraries["tool_definition_paths"]; ok {
			if err := json.Unmarshal(rawPaths, &layout.ToolDefinitionPaths); err != nil {
				return nil, nil, configError("%s: libraries.tool_definition_paths must be an array of strings", settingPath)
			}
		}
	}
	return layout, warnings, nil
}

// catalogFolderDefaults returns the folder of every catalog as the setting names it when
// `catalogs` does not (configured holds the ones it names). A setting in a ".jad" folder
// reads <that folder>/<catalog> (profiles, models, ...), the project's .jad/<catalog>. Until
// this was fixed the default was resolved from the setting's folder, <.jad>/.jad/<catalog>,
// so a user who has used those defaults has data in a doubled <.jad>/.jad folder. That folder
// is the legacy root: when it is a directory every default keeps resolving into it, all of
// them together (a root is never split over the two places), and a warning says so. Nothing
// is moved or created. A setting in any other folder keeps the .jad/<catalog> defaults.
//
// The returned map is a new one the caller may overwrite with the configured folders.
func catalogFolderDefaults(settingPath string, configured map[string]string) (map[string]string, []string) {
	folders := make(map[string]string, len(layoutCatalogDefaults))
	for key, value := range layoutCatalogDefaults {
		folders[key] = value
	}
	if len(configured) == len(layoutCatalogDefaults) {
		return folders, nil // every folder is named: no default is used
	}
	settingDir := filepath.Dir(settingPath)
	if absolute, err := filepath.Abs(settingDir); err != nil || filepath.Base(absolute) != ".jad" {
		return folders, nil
	}
	legacy := filepath.Join(settingDir, ".jad")
	info, err := os.Stat(legacy)
	var warnings []string
	switch {
	case err == nil && info.IsDir():
		warnings = append(warnings, legacyCatalogWarning(settingPath, settingDir, legacy, configured))
		return folders, warnings
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		// Unknown: keep what runs today rather than pick a place that may hide data.
		return folders, []string{fmt.Sprintf("%s: cannot tell whether %s exists (%v); the default catalog folders stay under it", settingPath, legacy, err)}
	}
	for key, value := range layoutCatalogDefaults {
		folders[key] = strings.TrimPrefix(value, ".jad/")
	}
	return folders, nil
}

// legacyCatalogWarning is the warning of a setting in a ".jad" folder that reads its default
// folders from the doubled <.jad>/.jad folder. It names the folders that also exist where the
// fixed default would read them, since then the data is in both places.
func legacyCatalogWarning(settingPath, settingDir, legacy string, configured map[string]string) string {
	var inBoth []string
	for _, key := range []string{"profiles", "models", "tools", "mcp", "remotes", "state", "sandbox"} {
		if _, named := configured[key]; named {
			continue
		}
		if _, err := os.Lstat(filepath.Join(settingDir, strings.TrimPrefix(layoutCatalogDefaults[key], ".jad/"))); err == nil {
			inBoth = append(inBoth, key)
		}
	}
	message := fmt.Sprintf("%s: the catalogs it does not name under `catalogs` are read from %s, the doubled .jad folder that older versions used as the default (the default is now %s/<catalog>); nothing is moved. To use the new folders, move the data yourself and remove %s, or name the folders in `catalogs`", settingPath, legacy, settingDir, legacy)
	if len(inBoth) > 0 {
		message += fmt.Sprintf(". Both places hold %s: only the doubled folder is read, so what is in %s/<catalog> is ignored", strings.Join(inBoth, ", "), settingDir)
	}
	return message
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
