package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// skipped fields differ by design: the layout names profiles and models by ID, and the
// setting file is another file.
var skippedFields = map[string]bool{
	"SettingPath": true, "Profile": true, "DefaultProfile": true, "PathProfiles": true,
	"ModelDefault": true, "Models": true,
	// The root folder moves with the tree. Skill, prompt and tool-definition paths were
	// written absolute into the new root setting, so nothing else depends on it.
	"GlobalJadRoot": true,
}

// verify loads every migrated profile from the new tree and compares it with the legacy
// load of the same profile, after the paths that moved are translated. Anything that
// differs is a Difference: the migration changed the behavior of that session.
func (r *runState) verify() {
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{RootSettingPath: filepath.Join(r.out, "setting.json"), Environment: r.environment()})
	if err != nil {
		r.report.Differences = append(r.report.Differences, fmt.Sprintf("the new tree does not load: %v", err))
		return
	}
	for _, item := range r.converted {
		replace := r.replacer(item.moved)
		label := fmt.Sprintf("%s:%s %s -> %s", item.root, item.session, nameOf(item.legacy), item.id)
		location, err := sessionfs.Resolve(loaded.Roots, item.root+":"+item.session)
		if err != nil {
			location, err = sessionfs.Resolve(loaded.Roots, item.session)
		}
		if err != nil {
			r.report.Differences = append(r.report.Differences, label+": "+err.Error())
			continue
		}
		migrated, err := sessionfs.LoadSessionSettings(location, item.id, "")
		if err != nil {
			r.report.Differences = append(r.report.Differences, fmt.Sprintf("%s: does not load: %v", label, err))
			continue
		}
		for _, difference := range compare(item.config, migrated, replace) {
			r.report.Differences = append(r.report.Differences, label+": "+difference)
		}
	}
}

func nameOf(legacy string) string {
	if legacy == "" {
		return "default"
	}
	return legacy
}

func (r *runState) environment() map[string]string {
	return map[string]string{}
}

// replacer returns a function that rewrites every legacy path in a string, longest path
// first so a file wins over the folder that holds it.
func (r *runState) replacer(profile map[string]string) func(string) string {
	all := make(map[string]string, len(r.pathMap)+len(profile))
	for from, to := range r.pathMap {
		all[from] = to
	}
	for from, to := range profile {
		all[from] = to
	}
	keys := make([]string, 0, len(all))
	for from := range all {
		keys = append(keys, from)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	pairs := make([]string, 0, len(keys)*2)
	for _, from := range keys {
		pairs = append(pairs, from, all[from])
	}
	replacer := strings.NewReplacer(pairs...)
	return replacer.Replace
}

// compare lists the fields of two loaded configurations that differ.
func compare(legacy, migrated sessionfs.SessionConfig, translate func(string) string) []string {
	before, err := fieldsOf(legacy)
	if err != nil {
		return []string{"cannot compare: " + err.Error()}
	}
	after, err := fieldsOf(migrated)
	if err != nil {
		return []string{"cannot compare: " + err.Error()}
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	var differences []string
	for _, name := range names {
		if skippedFields[name] {
			continue
		}
		if name == "MCPServers" {
			differences = append(differences, compareServers(legacy.MCPServers, migrated.MCPServers)...)
			continue
		}
		if name == "ToolDefinitionPaths" {
			// The root setting holds one list for every session, so a session may gain
			// definitions it did not list. They are used only by tools the toolset enables;
			// losing one would break a tool, so only that is a difference.
			if missing := missingPaths(legacy.ToolDefinitionPaths, migrated.ToolDefinitionPaths); len(missing) > 0 {
				differences = append(differences, "ToolDefinitionPaths lost "+strings.Join(missing, ", "))
			}
			continue
		}
		want := translate(string(before[name]))
		got := string(after[name])
		if want != got {
			differences = append(differences, fmt.Sprintf("%s differs\n      legacy:   %s\n      migrated: %s", name, clip(want), clip(got)))
		}
	}
	return differences
}

func fieldsOf(config sessionfs.SessionConfig) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	return fields, json.Unmarshal(data, &fields)
}

func clip(text string) string {
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

func missingPaths(want, have []string) []string {
	present := make(map[string]bool, len(have))
	for _, path := range have {
		present[path] = true
	}
	var missing []string
	for _, path := range want {
		if !present[path] {
			missing = append(missing, path)
		}
	}
	return missing
}

// compareServers checks each server by id: the same flags, the same denied tools, and a
// config file with the same content. The path itself changes, and two ids may have shared
// one legacy file, so a path translation cannot be used here.
func compareServers(legacy, migrated []sessionfs.MCPServerReference) []string {
	var differences []string
	if len(legacy) != len(migrated) {
		return []string{fmt.Sprintf("MCPServers: %d servers before, %d after", len(legacy), len(migrated))}
	}
	for index, before := range legacy {
		after := migrated[index]
		switch {
		case before.ID != after.ID:
			differences = append(differences, fmt.Sprintf("MCPServers[%d]: id %q became %q", index, before.ID, after.ID))
		case before.Enabled != after.Enabled:
			differences = append(differences, fmt.Sprintf("MCPServers %s: enabled %v became %v", before.ID, before.Enabled, after.Enabled))
		case before.Approval != after.Approval:
			differences = append(differences, fmt.Sprintf("MCPServers %s: approval %q became %q", before.ID, before.Approval, after.Approval))
		case strings.Join(before.DenyTools, "\x00") != strings.Join(after.DenyTools, "\x00"):
			differences = append(differences, fmt.Sprintf("MCPServers %s: deny_tools %v became %v", before.ID, before.DenyTools, after.DenyTools))
		case !sameFile(before.ConfigPath, after.ConfigPath):
			differences = append(differences, fmt.Sprintf("MCPServers %s: the config file %s does not match %s", before.ID, after.ConfigPath, before.ConfigPath))
		}
	}
	return differences
}

func sameFile(a, b string) bool {
	left, errLeft := os.ReadFile(a)
	right, errRight := os.ReadFile(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}
