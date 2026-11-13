package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// The private folders of a profile in the workspace layout (docs/design_workspace_layout.md
// §4.1): W/.jad/profile-own/<category>/<name> holds its private documents (and, from phase 3,
// models and toolsets); W/.jad/data/profiles/<category>/<name> its working data.
const (
	profileOwnFolder      = "profile-own"
	profileOwnPlaceholder = "{profile_own}"
)

var profileDataFolder = filepath.Join("data", "profiles")

// SettingLayer is one layer of settings a profile's effective settings are merged from.
type SettingLayer struct {
	Name   string
	Values map[string]json.RawMessage
}

// The names of the setting layers, in merge order.
const (
	SettingLayerBuiltIn   = "built-in"
	SettingLayerDefaults  = "defaults"
	SettingLayerGlobal    = "global"
	SettingLayerWorkspace = "workspace"
	SettingLayerSession   = "session"
	SettingLayerProfile   = "profile"
	SettingLayerToolset   = "toolset"
)

// ProfileOwnDir is W/.jad/profile-own/<category>/<name> of the profile id in the workspace
// layout, and "" for an id that is not a <category>/<name> name.
func (l Layout) ProfileOwnDir(id string) string {
	return l.profileFolder(profileOwnFolder, id)
}

// ProfileDataDir is W/.jad/data/profiles/<category>/<name> of the profile id in the
// workspace layout, and "" for an id that is not a <category>/<name> name.
func (l Layout) ProfileDataDir(id string) string {
	return l.profileFolder(profileDataFolder, id)
}

func (l Layout) profileFolder(folder, id string) string {
	relative := filepath.FromSlash(id)
	if l.Workspace == "" || strings.Count(id, "/") != 1 || !filepath.IsLocal(relative) {
		return ""
	}
	return filepath.Join(l.Workspace, ".jad", folder, relative)
}

// cutPlaceholder replaces a leading placeholder of document with value. A placeholder may be
// the whole document or be followed by a / and a path; anything else is an error.
func cutPlaceholder(document, placeholder, value string) (string, bool, error) {
	rest, found := strings.CutPrefix(document, placeholder)
	if !found {
		return document, false, nil
	}
	if rest != "" && !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, string(filepath.Separator)) {
		return "", true, fmt.Errorf("%s may only be followed by a / and a path", placeholder)
	}
	return value + rest, true, nil
}

// expandProfileOwn fills a leading {profile_own} of a profile's prompt documents with its own
// folder (workspace layout only; v1 keeps the text as it is).
func (l Layout) expandProfileOwn(documents []string, id, where string) ([]string, error) {
	own := l.ProfileOwnDir(id)
	if own == "" {
		return documents, nil
	}
	expanded := make([]string, len(documents))
	for index, document := range documents {
		value, _, err := cutPlaceholder(document, profileOwnPlaceholder, own)
		if err != nil {
			return nil, configError("%s: prompt_documents[%d]: %v", where, index, err)
		}
		expanded[index] = value
	}
	return expanded, nil
}

// checkPrivateDocument refuses a prompt document of the profile id that resolves, after
// links, into another profile's private folder of any known workspace. It is a rule of
// prompt assembly, not a read barrier: without a sandbox exec can still read the file. A
// document that cannot be resolved (missing) is left to the read, which reports it.
func (l Layout) checkPrivateDocument(id, document string) error {
	if len(l.ProfileOwnRoots) == 0 {
		return nil
	}
	real, err := filepath.EvalSymlinks(document)
	if err != nil {
		return nil
	}
	own := ""
	if dir := l.ProfileOwnDir(id); dir != "" {
		// The own folder by its name under the real profile-own root, not by its target: a
		// link from the own folder to another profile's must not make that one "own".
		own = filepath.Join(realPath(filepath.Dir(filepath.Dir(dir))), filepath.FromSlash(id))
	}
	for _, root := range l.ProfileOwnRoots {
		for _, candidate := range appendNewStrings([]string{filepath.Clean(root)}, realPath(root)) {
			if PathWithin(candidate, real) && (own == "" || !PathWithin(own, real)) {
				return fmt.Errorf("%s is in another profile's private folder (%s); only that profile may use it as a prompt document", document, real)
			}
		}
	}
	return nil
}

// checkPrivateDocuments applies checkPrivateDocument to every prompt document a loaded
// configuration of the profile id read: its own, its statecontext triggers', its agents' and
// those of its agent toolsets.
func (l Layout) checkPrivateDocuments(id, where string, config SessionConfig) error {
	if len(l.ProfileOwnRoots) == 0 {
		return nil
	}
	documents := append([]string(nil), config.PromptDocuments...)
	for _, trigger := range config.StateContext.Triggers {
		documents = append(documents, trigger.PromptDocuments...)
	}
	if subagent := config.NativeSubagent; subagent != nil {
		for _, agent := range subagent.Agents {
			documents = append(documents, agent.PromptDocuments...)
		}
	}
	for _, document := range documents {
		if err := l.checkPrivateDocument(id, document); err != nil {
			return configError("%s: prompt_documents: %v", where, err)
		}
	}
	for _, derived := range config.AgentToolsets {
		if err := l.checkPrivateDocuments(id, where, derived); err != nil {
			return err
		}
	}
	return nil
}

// judgePrivateDocuments is checkPrivateDocument for a profile's own prompt documents before
// its settings are composed, so a profile that names another's private document is left out
// with a warning unless it is the one selected. A relative document is resolved from the cwd
// the layers give, when that is absolute; the load of the selected profile checks again.
func (l Layout) judgePrivateDocuments(id, profilePath string, file layoutProfileFile, session layoutSessionFile) error {
	if len(l.ProfileOwnRoots) == 0 || len(file.PromptDocuments) == 0 {
		return nil
	}
	documents, err := l.expandProfileOwn(file.PromptDocuments, id, profilePath)
	if err != nil {
		return err
	}
	cwd := ""
	layers := mergeSettings(mergeSettings(l.Defaults, session.Settings), file.Settings)
	if session.CWD != "" && file.Settings["cwd"] == nil {
		layers["cwd"] = mustJSON(session.CWD)
	}
	if json.Unmarshal(layers["cwd"], &cwd) == nil {
		cwd = expandHome(strings.TrimSpace(cwd))
	}
	for index, document := range documents {
		if value, _, err := cutPlaceholder(document, workspacePlaceholder, l.Workspace); err == nil {
			document = value
		}
		document = expandHome(document)
		if !filepath.IsAbs(document) {
			if !filepath.IsAbs(cwd) {
				continue
			}
			document = filepath.Join(cwd, document)
		}
		if err := l.checkPrivateDocument(id, filepath.Clean(document)); err != nil {
			return configError("%s: prompt_documents[%d]: %v", profilePath, index, err)
		}
	}
	return nil
}

// profilePrivateRoots are the folders that hold every profile's private folders in the
// workspace, W/.jad/profile-own and W/.jad/data/profiles, as written and below the real
// W/.jad; nil for the global scope, which has no profiles.
func (l Layout) profilePrivateRoots() []string {
	if l.Workspace == "" {
		return nil
	}
	jad := filepath.Join(l.Workspace, ".jad")
	var roots []string
	for _, base := range appendNewStrings([]string{jad}, realPath(jad)) {
		roots = appendNewStrings(roots, filepath.Join(base, profileOwnFolder), filepath.Join(base, profileDataFolder))
	}
	return roots
}

// profileShown are the profile's own folder and data folder, as written and below the real
// W/.jad (never by the target of a link at the folder itself); nil in v1.
func (l Layout) profileShown(id string) []string {
	own, data := l.ProfileOwnDir(id), l.ProfileDataDir(id)
	if own == "" {
		return nil
	}
	jad := filepath.Join(l.Workspace, ".jad")
	real := realPath(jad)
	shown := []string{own, data}
	if real != jad {
		relative := filepath.FromSlash(id)
		shown = append(shown, filepath.Join(real, profileOwnFolder, relative), filepath.Join(real, profileDataFolder, relative))
	}
	return shown
}
