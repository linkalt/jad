package contentlibrary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
)

// The workspace layout (v2) gives the library its folders from global.json and workspace.json,
// read once at start (sessionfs.Libraries): no setting.json is read. The global folders are
// shared by every workspace, a workspace's own are seen only there. A skill or command name
// found in both scopes is ambiguous: it is left out of every list and naming it is an error
// that names both files, so neither scope wins silently (design Q3). Prompts and templates are
// picked by a path-derived ID, so the two scopes simply list side by side.

// ErrUnknownWorkspace is a library request for a workspace the server does not serve.
var ErrUnknownWorkspace = errors.New("workspace not found")

// AmbiguousError is a skill or command named in both the global and the workspace library.
// It is a ConfigError too: the person fixes it by renaming one.
type AmbiguousError struct{ ConfigError }

func ambiguousError(note string) error { return &AmbiguousError{ConfigError{Message: note}} }

// As lets errors.As find the ConfigError inside.
func (e *AmbiguousError) As(target any) bool {
	if config, ok := target.(**ConfigError); ok {
		*config = &e.ConfigError
		return true
	}
	return false
}

type scopedDirectories struct {
	global, workspace libraryDirectories
}

// scopedDirectoriesOf gives the folders of libraries: the global ones (the built-in defaults,
// from G, for a kind global.json does not set) and the workspace's own.
func scopedDirectoriesOf(libraries *sessionfs.Libraries) (scopedDirectories, error) {
	var defaults libraryDefaults
	if err := json.Unmarshal(embeddedDefaults, &defaults); err != nil {
		return scopedDirectories{}, fmt.Errorf("decode embedded root-library defaults: %w", err)
	}
	maxBytes := defaultMaxAssetBytes
	if libraries.MaxAssetBytes != 0 {
		if libraries.MaxAssetBytes < minMaxAssetBytes || libraries.MaxAssetBytes > maxMaxAssetBytes {
			return scopedDirectories{}, configError("global.json: libraries.max_asset_bytes must be an integer between %d and %d", minMaxAssetBytes, maxMaxAssetBytes)
		}
		maxBytes = libraries.MaxAssetBytes
	}
	pick := func(set, fallback []string) []string {
		if set != nil {
			return set
		}
		resolved := make([]string, 0, len(fallback))
		for _, value := range fallback {
			resolved = append(resolved, resolveLibraryPath(libraries.GlobalDir, value))
		}
		return resolved
	}
	global := libraryDirectories{
		prompts: pick(libraries.Global.Prompts, defaults.Prompts), templates: pick(libraries.Global.Templates, defaults.Templates),
		skills: pick(libraries.Global.Skills, defaults.Skills), commands: pick(libraries.Global.Commands, defaults.Commands),
		maxAssetBytes: maxBytes,
	}
	own := libraries.Workspace
	workspace := libraryDirectories{prompts: own.Prompts, templates: own.Templates, skills: own.Skills, commands: own.Commands, maxAssetBytes: maxBytes}
	return scopedDirectories{global: global, workspace: workspace}, nil
}

// all are the folders of both scopes, global first.
func (s scopedDirectories) all() libraryDirectories {
	join := func(a, b []string) []string { return append(append([]string(nil), a...), b...) }
	return libraryDirectories{
		prompts: join(s.global.prompts, s.workspace.prompts), templates: join(s.global.templates, s.workspace.templates),
		skills: join(s.global.skills, s.workspace.skills), commands: join(s.global.commands, s.workspace.commands),
		maxAssetBytes: s.global.maxAssetBytes,
	}
}

// ambiguousNote is the warning for a name found in both scopes.
func ambiguousNote(kind Kind, name string, sources ...string) string {
	return fmt.Sprintf("%s %s is in the global and the workspace library (%s); it is left out until one is renamed", kind, name, strings.Join(sources, ", "))
}

// listScoped lists one workspace's library. Skills and commands whose name is in both scopes
// are left out and named in Ambiguous.
func listScoped(ctx context.Context, dirs scopedDirectories) (Catalog, error) {
	all := dirs.all()
	catalog := Catalog{Prompts: []Summary{}, Templates: []Summary{}, Skills: []Summary{}, Commands: []Summary{}}
	for _, entry := range []struct {
		kind Kind
		dirs []string
		dst  *[]Summary
	}{{Prompt, all.prompts, &catalog.Prompts}, {Template, all.templates, &catalog.Templates}} {
		items, err := listKind(ctx, entry.kind, entry.dirs, all.maxAssetBytes)
		if err != nil {
			return Catalog{}, err
		}
		*entry.dst = items
	}
	keyOf := map[Kind]func(Summary) string{Skill: func(s Summary) string { return s.Name }, Command: func(s Summary) string { return s.Command }}
	merge := func(kind Kind, global, workspace []Summary) []Summary {
		key := keyOf[kind]
		inGlobal := map[string][]string{}
		for _, item := range global {
			inGlobal[key(item)] = append(inGlobal[key(item)], item.Source)
		}
		both := map[string][]string{}
		for _, item := range workspace {
			if sources, found := inGlobal[key(item)]; found {
				if _, seen := both[key(item)]; !seen {
					both[key(item)] = append([]string(nil), sources...)
				}
				both[key(item)] = append(both[key(item)], item.Source)
			}
		}
		merged := make([]Summary, 0, len(global)+len(workspace))
		for _, item := range append(append([]Summary(nil), global...), workspace...) {
			if _, ambiguous := both[key(item)]; !ambiguous {
				merged = append(merged, item)
			}
		}
		names := make([]string, 0, len(both))
		for name := range both {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			catalog.Ambiguous = append(catalog.Ambiguous, ambiguousNote(kind, name, both[name]...))
		}
		return merged
	}
	skills := [2][]Summary{}
	for index, folders := range [][]string{dirs.global.skills, dirs.workspace.skills} {
		items, err := listKind(ctx, Skill, folders, all.maxAssetBytes)
		if err != nil {
			return Catalog{}, err
		}
		skills[index] = items
	}
	catalog.Skills = merge(Skill, skills[0], skills[1])
	sort.SliceStable(catalog.Skills, func(i, j int) bool { return localeCompare(catalog.Skills[i].Name, catalog.Skills[j].Name) < 0 })
	commands := [2][]Summary{}
	for index, folders := range [][]string{dirs.global.commands, dirs.workspace.commands} {
		items, err := listCommands(ctx, folders, all.maxAssetBytes)
		switch {
		case err != nil && ctx.Err() != nil:
			return Catalog{}, ctx.Err()
		case err != nil:
			catalog.Commands, catalog.CommandsError = []Summary{}, err.Error()
			return catalog, nil
		}
		commands[index] = items
	}
	catalog.Commands = merge(Command, commands[0], commands[1])
	sort.SliceStable(catalog.Commands, func(i, j int) bool { return catalog.Commands[i].Command < catalog.Commands[j].Command })
	return catalog, nil
}

// workspaceLibraries picks the libraries of workspace ("" is the start workspace) from the
// options of a workspace-layout server.
func (o Options) workspaceLibraries(workspace string) (*sessionfs.Libraries, error) {
	if workspace == "" {
		workspace = o.StartWorkspace
	}
	libraries := o.Workspaces[workspace]
	if libraries == nil {
		return nil, ErrUnknownWorkspace
	}
	return libraries, nil
}

// readScoped reads one asset of a workspace-layout library by ID; a skill or command whose
// name is in both scopes is refused.
func readScoped(ctx context.Context, dirs scopedDirectories, kind Kind, id string) (Asset, bool, error) {
	all := dirs.all()
	asset, found, err := readByID(ctx, kind, directoryList(all, kind), id, all.maxAssetBytes)
	if err != nil || !found || (kind != Skill && kind != Command) {
		return asset, found, err
	}
	catalog, err := listScoped(ctx, dirs)
	if err != nil {
		return Asset{}, false, err
	}
	name := asset.Name
	if kind == Command {
		name = asset.Command
	}
	for _, note := range catalog.Ambiguous {
		if strings.HasPrefix(note, string(kind)+" "+name+" is in") {
			return Asset{}, false, ambiguousError(note)
		}
	}
	return asset, true, nil
}

// ListWorkspaceSkills is the skill tool's list in the workspace layout: the skills of both
// scopes (in a scope the first folder that has a name wins, as in v1), without the names
// found in both.
func ListWorkspaceSkills(ctx context.Context, libraries *sessionfs.Libraries) ([]Asset, error) {
	dirs, err := scopedDirectoriesOf(libraries)
	if err != nil {
		return nil, err
	}
	global, err := ListSkills(ctx, dirs.global.skills, dirs.global.maxAssetBytes)
	if err != nil {
		return nil, err
	}
	own, err := ListSkills(ctx, dirs.workspace.skills, dirs.workspace.maxAssetBytes)
	if err != nil {
		return nil, err
	}
	inGlobal := map[string]bool{}
	for _, skill := range global {
		inGlobal[skill.Name] = true
	}
	ambiguous := map[string]bool{}
	for _, skill := range own {
		if inGlobal[skill.Name] {
			ambiguous[skill.Name] = true
		}
	}
	skills := make([]Asset, 0, len(global)+len(own))
	for _, skill := range append(global, own...) {
		if !ambiguous[skill.Name] {
			skills = append(skills, skill)
		}
	}
	sort.SliceStable(skills, func(i, j int) bool { return localeCompare(skills[i].Name, skills[j].Name) < 0 })
	return skills, nil
}

// ReadWorkspaceSkill is the skill tool's read in the workspace layout: a name in both scopes
// is an error that names both files.
func ReadWorkspaceSkill(ctx context.Context, libraries *sessionfs.Libraries, name string) (LoadedSkill, bool, error) {
	dirs, err := scopedDirectoriesOf(libraries)
	if err != nil {
		return LoadedSkill{}, false, err
	}
	global, inGlobal, err := ReadSkill(ctx, dirs.global.skills, name, dirs.global.maxAssetBytes)
	if err != nil {
		return LoadedSkill{}, false, err
	}
	own, inWorkspace, err := ReadSkill(ctx, dirs.workspace.skills, name, dirs.workspace.maxAssetBytes)
	switch {
	case err != nil:
		return LoadedSkill{}, false, err
	case inGlobal && inWorkspace:
		return LoadedSkill{}, false, ambiguousError(ambiguousNote(Skill, name, global.Directory, own.Directory))
	case inWorkspace:
		return own, true, nil
	default:
		return global, inGlobal, nil
	}
}

// GlobalInstallFolders are the first global skill and command folders and the asset limit of
// a workspace-layout setting, where jad plugin installs (plugins are global).
func GlobalInstallFolders(libraries *sessionfs.Libraries) (skill, command string, maxBytes int64, err error) {
	dirs, err := scopedDirectoriesOf(libraries)
	if err != nil {
		return "", "", 0, err
	}
	if len(dirs.global.skills) > 0 {
		skill = dirs.global.skills[0]
	}
	if len(dirs.global.commands) > 0 {
		command = dirs.global.commands[0]
	}
	return skill, command, dirs.global.maxAssetBytes, nil
}
