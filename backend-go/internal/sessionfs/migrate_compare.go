package sessionfs

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// The side-by-side check of "jad migrate workspace --apply": the v1 loads taken before the
// first write against the loads of the workspace layout after workspace.json exists. It
// compares what a session means, not the v2 additions (sandbox rules, profile folders, self
// edit): identity, profiles, the model offer (each model file by content), the working folder,
// where the transcript and the staging live (relative to the session's state folder), the
// tool definitions, the MCP servers (each by content) and which tools are on. It is a guard
// against a path or a name that moved, not a proof that the two layouts behave alike.

// migrationPrint is the compared view of one (session, profile) load.
type migrationPrint map[string]string

// sessionPrints loads every migrated session in the layout of roots, once for its first
// profile and once for each other profile of its scope, and records the compared view.
func (p *migrationPlanner) sessionPrints(roots []Root, rootID string, layout Layout, sessions []string) map[string]migrationPrint {
	prints := map[string]migrationPrint{}
	for _, id := range sessions {
		location, err := Resolve(roots, rootID+":"+id)
		if err != nil {
			prints[id+"/"] = migrationPrint{"load": "error"}
			continue
		}
		base, err := LoadLayoutSessionSettings(location, layout, "", "")
		if err != nil {
			prints[id+"/"] = migrationPrint{"load": "error"}
			continue
		}
		prints[id+"/"+base.Profile] = printOf(base)
		for _, profile := range base.Profiles {
			if profile == base.Profile {
				continue
			}
			config, err := LoadLayoutSessionSettings(location, layout, profile, "")
			if err != nil {
				prints[id+"/"+profile] = migrationPrint{"load": "error"}
				continue
			}
			prints[id+"/"+profile] = printOf(config)
		}
	}
	return prints
}

func printOf(config SessionConfig) migrationPrint {
	print := migrationPrint{
		"session": config.SessionName, "category": config.Category, "profile": config.Profile,
		"default_profile": config.DefaultProfile, "profiles": strings.Join(config.Profiles, ","),
		"model_default": config.ModelDefault, "models": strings.Join(sortedCopy(config.Models), ","),
		"cwd": config.WorkingDir, "tool_definition_paths": strings.Join(config.ToolDefinitionPaths, ","),
		"messages": inState(config, config.MessagesPath), "messages_backup": inState(config, config.MessagesBackupPath),
		"usage_cache": inState(config, config.UsageCachePath), "staging": inState(config, config.StagingDir),
		"tools": toolSurface(config),
	}
	names, _ := json.Marshal(config.ProfileNames)
	print["profile_names"] = string(names)
	ids := make([]string, 0, len(config.ModelPaths))
	for id := range config.ModelPaths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		print["model "+id] = contentOf(config.ModelPaths[id])
	}
	// Where the session writes. A path that follows the state folder compares as {state}/...; one
	// that does not (an absolute tool_storage_dir into the v1 state, say) keeps its place in v2
	// and differs: v1 and v2 would then write the same files.
	targets := map[string][]string{}
	for _, target := range config.writeTargets() {
		path := target.path
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) && filepath.IsAbs(config.WorkingDir) {
			path = filepath.Join(config.WorkingDir, path)
		}
		targets["write "+target.setting] = append(targets["write "+target.setting], inState(config, path))
	}
	for key, paths := range targets {
		print[key] = strings.Join(paths, ",")
	}
	for _, server := range config.MCPServers {
		print["mcp "+server.ID] = fmt.Sprintf("%t %s %t %t %s", server.Enabled, strings.Join(sortedCopy(server.DenyTools), ","), server.Resources, server.Prompts, contentOf(server.ConfigPath))
	}
	return print
}

func sortedCopy(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}

// inState names a path below the session's state folder by its place in it, so the v1 and the
// v2 state folders compare equal; a path elsewhere is compared as it is.
func inState(config SessionConfig, path string) string {
	if path == "" {
		return ""
	}
	if config.StateDir != "" && PathWithin(config.StateDir, path) {
		relative, _ := filepath.Rel(config.StateDir, path)
		return "{state}/" + filepath.ToSlash(relative)
	}
	return path
}

// contentOf is the hash of a file, so a model or MCP file compares by what it says, not where
// it is; a file that cannot be read compares as unreadable.
func contentOf(path string) string {
	if path == "" {
		return ""
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return "unreadable"
	}
	return sum
}

// toolSurface lists the tools that are on: the Native* and Extension* settings that are set.
func toolSurface(config SessionConfig) string {
	value := reflect.ValueOf(config)
	var names []string
	for index := 0; index < value.NumField(); index++ {
		name := value.Type().Field(index).Name
		if !strings.HasPrefix(name, "Native") && !strings.HasPrefix(name, "Extension") {
			continue
		}
		if field := value.Field(index); field.Kind() == reflect.Pointer && !field.IsNil() {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

// compare loads the workspace layout of W and compares it with the v1 loads. It returns the
// differences, the number of pairs compared and notes.
func (a *migrationApplier) compare() (differences []string, compared int, notes []string) {
	p := a.p
	env := map[string]string{}
	for key, value := range p.env {
		if key != "JAD_ROOT_SETTING_PATH" { // it keeps v1 even when workspace.json exists
			env[key] = value
		}
	}
	config, err := Load(LoadOptions{WorkingDir: p.workspace, Environment: env, GlobalDir: p.options.GlobalDir})
	if err != nil {
		return []string{fmt.Sprintf("the workspace layout does not load: %v", err)}, 0, nil
	}
	if len(config.Workspaces) == 0 {
		return []string{"starting in the workspace did not select the workspace layout"}, 0, nil
	}
	var root *Root
	for index := range config.Roots {
		if config.Roots[index].ID == p.root.ID {
			root = &config.Roots[index]
		}
	}
	if root == nil || root.Layout == nil {
		return []string{fmt.Sprintf("the workspace layout has no session root %s", p.root.ID)}, 0, nil
	}
	if !sameEncoding(p.config.Server, config.Server) {
		differences = append(differences, "the server settings differ")
	}
	if !sameEncoding(p.config.CLI, config.CLI) {
		differences = append(differences, "the cli settings differ")
	}
	v2 := p.sessionPrints(config.Roots, root.ID, *root.Layout, p.sessions)
	keys := make([]string, 0, len(a.v1))
	for key := range a.v1 {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		old, now := a.v1[key], v2[key]
		compared++
		if now == nil {
			differences = append(differences, fmt.Sprintf("%s: the workspace layout does not list it", key))
			continue
		}
		fields := map[string]struct{}{}
		for field := range old {
			fields[field] = struct{}{}
		}
		for field := range now {
			fields[field] = struct{}{}
		}
		names := make([]string, 0, len(fields))
		for field := range fields {
			names = append(names, field)
		}
		sort.Strings(names)
		for _, field := range names {
			if old[field] != now[field] {
				differences = append(differences, fmt.Sprintf("%s: %s differs (v1 %q, workspace layout %q)", key, field, old[field], now[field]))
			}
		}
	}
	for key := range v2 {
		if _, ok := a.v1[key]; !ok {
			differences = append(differences, fmt.Sprintf("%s: only the workspace layout lists it", key))
		}
	}
	if len(a.v1) == 0 {
		notes = append(notes, "no session was loaded to compare")
	}
	return differences, compared, notes
}

func sameEncoding(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
