package sessionfs

import (
	"encoding/json"
	"slices"
	"strings"
)

// The content libraries of the workspace layout (design Q3, decider fix B): global.json's
// libraries, from G, are shared by every workspace; workspace.json's, from W/.jad, are seen
// only in that workspace. Both are read once at start; no setting.json is read.

// LibraryFolders are the folders of one scope by kind (libraries.prompt_dirs, template_dirs,
// skill_dirs, command_dirs), absolute. A nil list is not set, so the built-in default applies
// (global scope only); an empty one is set to no folders.
type LibraryFolders struct {
	Prompts, Templates, Skills, Commands []string
}

// Libraries are the content libraries a session or the HTTP library of a workspace sees.
type Libraries struct {
	// GlobalDir is G, the base of the built-in default folders of the global scope.
	GlobalDir string
	Global    LibraryFolders
	// Workspace are the workspace's own folders; a skill or command name found in both
	// scopes is ambiguous and is never picked silently.
	Workspace LibraryFolders
	// MaxAssetBytes is global.json's libraries.max_asset_bytes; 0 when it is not set.
	MaxAssetBytes int64
}

// libraryFolderKeys are the folder keys of a libraries object, in LibraryFolders order.
var libraryFolderKeys = []string{"prompt_dirs", "template_dirs", "skill_dirs", "command_dirs"}

// parseLibraryFolders reads the folder keys of a libraries object, each resolved from base.
// The other keys are the caller's; one it does not allow is refused.
func parseLibraryFolders(libraries map[string]json.RawMessage, where, base string, other ...string) (LibraryFolders, error) {
	for key := range libraries {
		if !containsString(libraryFolderKeys, key) && !containsString(other, key) {
			return LibraryFolders{}, configError("%s: libraries.%s is not a key here (known: %s)", where, key, strings.Join(append(append([]string(nil), libraryFolderKeys...), other...), ", "))
		}
	}
	var folders LibraryFolders
	targets := []*[]string{&folders.Prompts, &folders.Templates, &folders.Skills, &folders.Commands}
	for index, key := range libraryFolderKeys {
		raw, exists := libraries[key]
		if !exists {
			continue
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return LibraryFolders{}, configError("%s: libraries.%s must be an array of folders", where, key)
		}
		resolved := make([]string, 0, len(values))
		for position, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return LibraryFolders{}, configError("%s: libraries.%s[%d] must be a non-empty string", where, key, position)
			}
			resolved = appendNewStrings(resolved, resolveFrom(base, value))
		}
		*targets[index] = resolved
	}
	return folders, nil
}

// librariesObject is a file's libraries object; nil when it has none.
func librariesObject(setting map[string]json.RawMessage, where string) (map[string]json.RawMessage, error) {
	raw, exists := setting["libraries"]
	if !exists {
		return nil, nil
	}
	var libraries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &libraries); err != nil || libraries == nil {
		return nil, configError("%s: libraries must be an object", where)
	}
	return libraries, nil
}

// workspaceLibraries reads workspace.json's libraries: the folder keys and
// tool_definition_paths (4c), each resolved from W/.jad. The tool definition paths are the
// workspace's own list, read after the global one.
func workspaceLibraries(setting map[string]json.RawMessage, where, jad string) (LibraryFolders, []string, error) {
	libraries, err := librariesObject(setting, where)
	if err != nil || libraries == nil {
		return LibraryFolders{}, nil, err
	}
	folders, err := parseLibraryFolders(libraries, where, jad, "tool_definition_paths")
	if err != nil {
		return LibraryFolders{}, nil, err
	}
	raw, exists := libraries["tool_definition_paths"]
	if !exists {
		return folders, nil, nil
	}
	paths, err := parseLibraryDefinitionPaths(raw, where, jad)
	if err != nil {
		return LibraryFolders{}, nil, err
	}
	return folders, paths, nil
}

// checkToolDefinitionPaths refuses a tool definition file, from global.json or workspace.json,
// that lies in a place the loader keeps away from a configuration file (4c): the folders the
// fixed hidden list names (G/auth and the other workspaces' .jad), and the folders a profile
// writes (its own documents and history, the profile catalog and the working data of the
// profiles), since a profile could then change what the tools are. A path is judged as written
// and after the links of the part that exists. The workspace layout only.
func (l Layout) checkToolDefinitionPaths(dataRoots []string) error {
	forbidden := append(append(slices.Clone(l.FixedHidePaths), l.selfEditPaths()...), dataRoots...)
	for _, path := range l.ToolDefinitionPaths {
		resolved := resolveExisting(path)
		for _, root := range forbidden {
			if within(path, root) || within(resolved, root) {
				return configError("libraries.tool_definition_paths: %s is in %s, which tool definitions may not be read from (the hidden folders, and the folders a profile writes)", path, root)
			}
		}
	}
	return nil
}
