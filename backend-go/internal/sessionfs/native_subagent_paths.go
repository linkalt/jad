package sessionfs

import (
	"encoding/json"
	"strings"
)

// NativeSubagentPaths narrows where one agent's write and edit may change files. It can
// only narrow: a path must still pass the session's own write and edit rules, and then
// this list as well. A relative entry is taken from the workspace root of the call, so
// an agent folder can name "src" without knowing where it will run.
type NativeSubagentPaths struct {
	// Allow, when not empty, limits the agent to paths inside one of its entries.
	Allow []string
	// Deny refuses any path inside one of its entries, even one Allow permits.
	Deny []string
}

// Set reports whether the agent has a paths list at all.
func (p NativeSubagentPaths) Set() bool { return len(p.Allow) > 0 || len(p.Deny) > 0 }

// optionalSubagentPaths reads the agent's paths: an object with allow and deny lists of
// path strings. Absent or null means no narrowing.
func optionalSubagentPaths(fields map[string]json.RawMessage, settingPath, section string) (NativeSubagentPaths, error) {
	raw, exists := fields["paths"]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return NativeSubagentPaths{}, nil
	}
	object, err := decodeConfigObject(raw, settingPath+": "+section+".paths")
	if err != nil {
		return NativeSubagentPaths{}, err
	}
	for name := range object {
		if name != "allow" && name != "deny" {
			return NativeSubagentPaths{}, configError("%s: %s.paths.%s is not supported; use allow and deny", settingPath, section, name)
		}
	}
	var paths NativeSubagentPaths
	if paths.Allow, err = subagentPathList(object, "allow", settingPath, section); err != nil {
		return NativeSubagentPaths{}, err
	}
	if paths.Deny, err = subagentPathList(object, "deny", settingPath, section); err != nil {
		return NativeSubagentPaths{}, err
	}
	if !paths.Set() {
		return NativeSubagentPaths{}, configError("%s: %s.paths needs at least one allow or deny entry", settingPath, section)
	}
	return paths, nil
}

func subagentPathList(object map[string]json.RawMessage, name, settingPath, section string) ([]string, error) {
	raw, exists := object[name]
	if !exists || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, configError("%s: %s.paths.%s must be an array of path strings", settingPath, section, name)
	}
	cleaned := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, configError("%s: %s.paths.%s[%d] must be a non-empty string", settingPath, section, name, index)
		}
		entry = expandHome(entry)
		if _, duplicate := seen[entry]; duplicate {
			return nil, configError("%s: %s.paths.%s lists %s twice", settingPath, section, name, entry)
		}
		seen[entry] = struct{}{}
		cleaned = append(cleaned, entry)
	}
	return cleaned, nil
}

// checkSubagentPaths refuses a paths list on an agent it cannot protect. write and edit
// are checked per path; a shell can touch any path, so bash and exec cannot be confined
// by a list of paths and the combination is an error instead of a false promise.
func checkSubagentPaths(agent NativeSubagentAgent, settingPath, section string) error {
	if !agent.Paths.Set() {
		return nil
	}
	var shells []string
	editors := false
	for _, name := range agent.Tools {
		switch name {
		case "bash", "exec", "process":
			shells = append(shells, name)
		case "write", "edit":
			editors = true
		}
	}
	if len(shells) > 0 {
		return configError("%s: %s lists %s together with paths; a shell can change any path, so paths cannot confine it: remove paths or the shell tool (process starts a shell command too)", settingPath, section, strings.Join(shells, ", "))
	}
	if !editors {
		return configError("%s: %s.paths has nothing to narrow: the agent lists neither write nor edit", settingPath, section)
	}
	return nil
}
