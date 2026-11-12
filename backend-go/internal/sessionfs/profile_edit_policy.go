package sessionfs

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// selfEditTopKeys are the keys of a profile file a self-edit may change at every level but
// off: what it is called, its instructions and documents (prompt_documents also pass the D3
// path limits), the models and tools it offers from what the workspace already shows it.
var selfEditTopKeys = []string{"schema_version", "description", "name", "prompt_role", "instructions", "prompt_documents", "models", "model_default", "toolset", "tools"}

// selfEditTuningSettings are the settings a self-edit may change under on: how the profile's
// runs are tuned. Every other setting (ceilings, sandbox, budget, hooks, env_file, cwd,
// tool_storage_dir, plan_mode, session_files, statecontext, checkpoint, persistence,
// image_store, http_debug, project_instructions) only under full (decider S5).
var selfEditTuningSettings = []string{"compaction", "front_compaction", "max_parallel_tool_calls", "tool_call_scheduling", "max_tool_rounds", "provider_retry", "provider_stream", "provider_network", "image_resize", "model_fallback"}

// SelfEditableKeys lists what a self-edit may change at level, for profile config. Under full
// every key of a profile file may change; the load checks still apply.
func SelfEditableKeys(level string) []string {
	switch level {
	case ProfileSelfEditOn:
		keys := slices.Clone(selfEditTopKeys)
		for _, setting := range selfEditTuningSettings {
			keys = append(keys, "settings."+setting)
		}
		return keys
	case ProfileSelfEditFull:
		return append(slices.Clone(selfEditTopKeys), "settings")
	}
	return nil
}

// selfEditPlace is what the D3 limits of a profile's prompt documents are judged against.
type selfEditPlace struct {
	layout  Layout
	profile string
	// cwd is the session cwd the candidate loads with: a relative document is read from it.
	cwd string
}

// checkSelfEditPolicy compares the profile file as it is (current) with what a self-edit or a
// revert would make it (candidate), key by key, and refuses a change the level does not allow
// (decider S5). It returns the changed keys, settings by name (settings.<key>). The trial load
// is a separate check: a change the policy allows must still load.
func checkSelfEditPolicy(level string, current, candidate []byte, place selfEditPlace) ([]string, error) {
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(current, &before); err != nil {
		return nil, fmt.Errorf("the profile file is not a JSON object: %w", err)
	}
	if err := json.Unmarshal(candidate, &after); err != nil || after == nil {
		return nil, &ClientError{Message: "the new profile file must be a JSON object"}
	}
	var changed []string
	for _, key := range slices.Sorted(maps.Keys(union(before, after))) {
		if sameValue(before[key], after[key]) {
			continue
		}
		if key == "settings" {
			settings, err := changedSettings(before[key], after[key])
			if err != nil {
				return nil, err
			}
			for _, setting := range settings {
				if level != ProfileSelfEditFull && !slices.Contains(selfEditTuningSettings, setting) {
					return nil, &ClientError{Message: fmt.Sprintf("settings.%s may not be changed by the profile itself: profile_self_edit is %s, and only full allows it (tuning settings it may change: %s)", setting, level, strings.Join(selfEditTuningSettings, ", "))}
				}
				changed = append(changed, "settings."+setting)
			}
			if err := checkSelfEditEnvFiles(after[key], place); err != nil {
				return nil, err
			}
			if err := checkSelfEditSettingPaths(after[key], settings, place); err != nil {
				return nil, err
			}
			continue
		}
		if !slices.Contains(selfEditTopKeys, key) {
			return nil, &ClientError{Message: fmt.Sprintf("%s is not a key of a profile file", key)}
		}
		if key == "prompt_documents" {
			if err := checkSelfEditDocuments(level, before[key], after[key], place); err != nil {
				return nil, err
			}
		}
		changed = append(changed, key)
	}
	return changed, nil
}

// changedSettings are the settings keys whose values differ.
func changedSettings(before, after json.RawMessage) ([]string, error) {
	var old, updated map[string]json.RawMessage
	if len(before) > 0 && json.Unmarshal(before, &old) != nil {
		return nil, fmt.Errorf("the profile file's settings are not an object")
	}
	if len(after) > 0 && string(after) != "null" && json.Unmarshal(after, &updated) != nil {
		return nil, &ClientError{Message: "settings must be an object"}
	}
	var changed []string
	for _, key := range slices.Sorted(maps.Keys(union(old, updated))) {
		if !sameValue(old[key], updated[key]) {
			changed = append(changed, key)
		}
	}
	return changed, nil
}

// checkSelfEditDocuments applies the D3 limits (design §4.5) to each prompt document the
// candidate adds; documents the file already names are left as they are, since the user wrote
// them. Under on a new document must be in the profile's own {profile_own}/documents, or in W
// but not in W/.jad other than W/.jad/documents; under full anywhere but G/auth. At every level
// another profile's private folder is refused (the load refuses it too).
func checkSelfEditDocuments(level string, before, after json.RawMessage, place selfEditPlace) error {
	var old, documents []string
	_ = json.Unmarshal(before, &old)
	if len(after) == 0 {
		// The key is removed: no document is added.
		return nil
	}
	if err := json.Unmarshal(after, &documents); err != nil {
		return &ClientError{Message: "prompt_documents must be an array of strings"}
	}
	for index, document := range documents {
		if slices.Contains(old, document) {
			continue
		}
		resolved, err := place.resolveDocument(level, document)
		if err != nil {
			return &ClientError{Message: fmt.Sprintf("prompt_documents[%d] %q: %v", index, document, err)}
		}
		if err := place.allowedPath(level, resolved); err != nil {
			return &ClientError{Message: fmt.Sprintf("prompt_documents[%d] %q: %v", index, document, err)}
		}
	}
	return nil
}

// resolveDocument is where a prompt document of the candidate is read from, after links as
// far as the path exists. Under on a ~ path is refused before it is expanded.
func (p selfEditPlace) resolveDocument(level, document string) (string, error) {
	document = strings.TrimSpace(document)
	if level != ProfileSelfEditFull && (document == "~" || strings.HasPrefix(document, "~/")) {
		return "", fmt.Errorf("a path in the home folder needs profile_self_edit full")
	}
	value, _, err := cutPlaceholder(document, profileOwnPlaceholder, p.layout.ProfileOwnDir(p.profile))
	if err != nil {
		return "", err
	}
	if value, _, err = cutPlaceholder(value, workspacePlaceholder, p.layout.Workspace); err != nil {
		return "", err
	}
	value = expandHome(value)
	if !filepath.IsAbs(value) {
		if !filepath.IsAbs(p.cwd) {
			return "", fmt.Errorf("a relative path needs an absolute session cwd")
		}
		value = filepath.Join(p.cwd, value)
	}
	return resolveExisting(filepath.Clean(value)), nil
}

// allowedPath refuses a resolved path the level does not let a self-edit name.
func (p selfEditPlace) allowedPath(level, resolved string) error {
	if p.layout.GlobalDir != "" && within(resolved, filepath.Join(p.layout.GlobalDir, "auth")) {
		return fmt.Errorf("G/auth is never named by a self-edit")
	}
	if err := p.layout.checkPrivateDocument(p.profile, resolved); err != nil {
		return err
	}
	if level == ProfileSelfEditFull {
		return nil
	}
	own := p.layout.ProfileOwnDir(p.profile)
	if own != "" && within(resolved, filepath.Join(own, "documents")) {
		return nil
	}
	jad := filepath.Join(p.layout.Workspace, ".jad")
	switch {
	case !within(resolved, p.layout.Workspace):
		return fmt.Errorf("under profile_self_edit on a document must be in the workspace %s or in {profile_own}/documents", p.layout.Workspace)
	case within(resolved, jad) && !within(resolved, filepath.Join(jad, "documents")):
		return fmt.Errorf("under profile_self_edit on a document in %s must be in %s/documents or {profile_own}/documents", jad, jad)
	}
	return nil
}

// checkSelfEditEnvFiles refuses an env_file of the candidate's settings, or of its hooks, that
// lies in G/auth, at every level: a self-edit never names the credentials folder.
func checkSelfEditEnvFiles(raw json.RawMessage, place selfEditPlace) error {
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil || place.layout.GlobalDir == "" {
		return nil
	}
	values := []json.RawMessage{settings["env_file"]}
	var hooks map[string]json.RawMessage
	if json.Unmarshal(settings["hooks"], &hooks) == nil {
		values = append(values, hooks["env_file"])
	}
	auth := filepath.Join(place.layout.GlobalDir, "auth")
	for _, value := range values {
		var path string
		if json.Unmarshal(value, &path) != nil || strings.TrimSpace(path) == "" {
			continue
		}
		path = expandHome(strings.TrimSpace(path))
		if !filepath.IsAbs(path) {
			path = filepath.Join(place.layout.Workspace, ".jad", path)
		}
		if within(resolveExisting(filepath.Clean(path)), auth) {
			return &ClientError{Message: "an env_file in G/auth is never named by a self-edit"}
		}
	}
	return nil
}

// checkSelfEditSettingPaths refuses a changed setting, at every level, that names a path in a
// profile-own folder of any known workspace (3b D3): tool storage, session files, writable
// paths and the like would let an in-process writer or a sandbox bind reach the history and
// the documents around the lock and the history. Every string of the setting is judged as a
// path ({profile_own}, {workspace} and ~ expanded, a relative one from the session cwd and from
// W/.jad), so a string that is not a path is refused only if it lands there.
func checkSelfEditSettingPaths(raw json.RawMessage, changed []string, place selfEditPlace) error {
	var settings map[string]json.RawMessage
	if len(changed) == 0 || json.Unmarshal(raw, &settings) != nil {
		return nil
	}
	roots := append([]string{filepath.Join(place.layout.Workspace, ".jad", profileOwnFolder)}, place.layout.ProfileOwnRoots...)
	for _, setting := range changed {
		var value any
		if json.Unmarshal(settings[setting], &value) != nil {
			continue
		}
		for _, text := range jsonStrings(value, nil) {
			for _, path := range place.settingPaths(text) {
				for _, root := range roots {
					if within(path, root) {
						return &ClientError{Message: fmt.Sprintf("settings.%s names %q, in a profile-own folder (%s): a self-edit never points a setting there; own documents change through edit documents", setting, text, root)}
					}
				}
			}
		}
	}
	return nil
}

// settingPaths are the places a setting string could name, after links as far as they exist.
func (p selfEditPlace) settingPaths(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	value, _, err := cutPlaceholder(text, profileOwnPlaceholder, p.layout.ProfileOwnDir(p.profile))
	if err != nil {
		return nil
	}
	if value, _, err = cutPlaceholder(value, workspacePlaceholder, p.layout.Workspace); err != nil {
		return nil
	}
	value = expandHome(value)
	if filepath.IsAbs(value) {
		return []string{resolveExisting(filepath.Clean(value))}
	}
	var paths []string
	for _, base := range []string{p.cwd, filepath.Join(p.layout.Workspace, ".jad")} {
		if filepath.IsAbs(base) {
			paths = append(paths, resolveExisting(filepath.Join(base, value)))
		}
	}
	return paths
}

// jsonStrings appends every string of a decoded JSON value, keys of objects included.
func jsonStrings(value any, out []string) []string {
	switch typed := value.(type) {
	case string:
		out = append(out, typed)
	case []any:
		for _, item := range typed {
			out = jsonStrings(item, out)
		}
	case map[string]any:
		for key, item := range typed {
			out = jsonStrings(item, append(out, key))
		}
	}
	return out
}

// within reports whether path is root or below it, comparing both as written and after links.
func within(path, root string) bool {
	return PathWithin(root, path) || PathWithin(realPath(root), path)
}

// resolveExisting resolves the links of the longest part of path that exists and keeps the
// rest as written, so a path that is not there yet is judged by where it would be.
func resolveExisting(path string) string {
	rest := ""
	for current := path; ; current = filepath.Dir(current) {
		if real, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(real, rest)
		}
		if _, err := os.Lstat(current); err == nil {
			// A dangling link: judged where it points cannot be known, so by its own name.
			return path
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
	}
}

// union is the keys of both maps.
func union(a, b map[string]json.RawMessage) map[string]json.RawMessage {
	all := make(map[string]json.RawMessage, len(a)+len(b))
	for key := range a {
		all[key] = nil
	}
	for key := range b {
		all[key] = nil
	}
	return all
}

// sameValue reports whether two optional JSON values are equal; a missing value equals only a
// missing one.
func sameValue(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return sameJSON(a, b)
}
