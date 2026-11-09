package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"jad/backend-go/internal/catalog"
)

// Sandbox templates are files <catalogs.sandbox>/<name>.json, each one sandbox object named by
// any number of profiles, sessions, root defaults and sub-agents (docs/isolation-sandbox-design.md
// §14). A sandbox value is a template name, an object {"template": <name>, ...overrides}, or an
// inline object as before.

// sandboxTemplateHeaderKeys are the catalog file keys of a template that are not sandbox keys.
var sandboxTemplateHeaderKeys = []string{"schema_version", "description"}

// SandboxTemplateFileSuffix ends the file name of a sandbox template.
const SandboxTemplateFileSuffix = ".json"

// setSandboxCatalog reads catalogs.sandbox: an absolute path (~/ allowed), a path relative to
// the root setting's folder, or {cwd}/<rest>, taken from each session's cwd. Any other
// placeholder is refused.
func (l *Layout) setSandboxCatalog(value, base, settingPath string) error {
	if rest, fromCWD := strings.CutPrefix(value, sandboxCatalogCWD); fromCWD {
		rest = strings.TrimPrefix(rest, string(filepath.Separator))
		if strings.Contains(rest, "{") || (rest != "" && filepath.IsAbs(rest)) {
			return configError("%s: catalogs.sandbox: {cwd} may only start the path, and no other placeholder is known; use an absolute path, a path relative to this file, or {cwd}/<folder>", settingPath)
		}
		l.SandboxFromCWD = filepath.Clean(rest)
		return nil
	}
	if strings.Contains(value, "{") {
		return configError("%s: catalogs.sandbox: only {cwd} is known, at the start; use an absolute path, a path relative to this file, or {cwd}/<folder>", settingPath)
	}
	l.Sandbox = resolveFrom(base, expandHome(value))
	return nil
}

// SandboxTemplateDir is the folder of the sandbox templates for a session whose cwd is cwd:
// catalogs.sandbox, with {cwd} filled in from the session's own cwd (never a sub-agent's
// worktree, whose copy of the folder is the child's own files).
func (l Layout) SandboxTemplateDir(cwd string) string {
	if l.Sandbox != "" || l.SandboxFromCWD == "" {
		return l.Sandbox
	}
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, l.SandboxFromCWD)
}

// sandboxTemplates reads the templates of one session's folder; each file read is stamped
// first, so an edited template reloads the sessions that use it.
type sandboxTemplates struct {
	dir   string
	scans *CatalogScans
}

// read returns the sandbox fields of template name, without the file header, with
// "template" set to the name for messages and the tool description.
func (t sandboxTemplates) read(name, where string) (map[string]json.RawMessage, error) {
	fields, _, err := t.readDescribed(name, where)
	return fields, err
}

// readDescribed is read plus the template's description ("" when it has none).
func (t sandboxTemplates) readDescribed(name, where string) (map[string]json.RawMessage, string, error) {
	if t.dir == "" {
		return nil, "", configError("%s: sandbox template %q: the session has no sandbox template folder (catalogs.sandbox)", where, name)
	}
	if strings.ContainsAny(name, `/\`) {
		return nil, "", configError("%s: sandbox template %q: templates are flat files <folder>/<name>.json; a sub-folder is not allowed", where, name)
	}
	if err := catalog.ValidateIdentifier(name); err != nil {
		return nil, "", configError("%s: sandbox template name %v", where, err)
	}
	path := filepath.Join(t.dir, name+SandboxTemplateFileSuffix)
	t.scans.noteFile(path)
	contents, err := readRegularFile(path, maxInternalJSONFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", configError("%s: sandbox template %q is not in %s%s", where, name, t.dir, templateSuggestion(name, ListSandboxTemplates(t.dir)))
	}
	if err != nil {
		return nil, "", configError("%s: could not be read: %v", path, err)
	}
	var header struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(contents, &header)
	fields, err := parseSandboxTemplate(path, contents)
	return fields, header.Description, err
}

// parseSandboxTemplate checks one template file: an object with enabled set (D19), no
// template of its own (D17), schema_version 1 when given.
func parseSandboxTemplate(path string, contents []byte) (map[string]json.RawMessage, error) {
	if strings.TrimSpace(string(contents)) == "" {
		return nil, configError("%s: expected a JSON object", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil || fields == nil {
		return nil, configError("%s: a sandbox template must be a JSON object", path)
	}
	if raw, has := fields["schema_version"]; has && strings.TrimSpace(string(raw)) != "1" {
		return nil, configError("%s: unsupported schema_version %s", path, strings.TrimSpace(string(raw)))
	}
	if _, has := fields["template"]; has {
		return nil, configError("%s: template is not allowed in a sandbox template: a template cannot name another", path)
	}
	if raw, has := fields["enabled"]; !has || strings.TrimSpace(string(raw)) == "null" {
		return nil, configError("%s: a sandbox template must set enabled (true or false)", path)
	}
	for _, key := range sandboxTemplateHeaderKeys {
		delete(fields, key)
	}
	if err := canonicalSandboxSpelling(fields, path); err != nil {
		return nil, err
	}
	// Every key is checked here, so a typo in a template names its file.
	if _, _, err := parseSandbox(mustJSON(fields), path, "sandbox"); err != nil {
		return nil, err
	}
	return fields, nil
}

// ListSandboxTemplates are the template names in dir, sorted; a missing folder has none.
func ListSandboxTemplates(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), SandboxTemplateFileSuffix)
		if !ok || strings.HasPrefix(entry.Name(), ".") || entry.IsDir() || catalog.ValidateIdentifier(name) != nil {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func templateSuggestion(name string, names []string) string {
	if len(names) == 0 {
		return " (the folder holds no template)"
	}
	if closest, distance := closestToolKey(name, names); closest != "" && distance <= 2 {
		return "; did you mean " + closest + "?"
	}
	return " (templates: " + strings.Join(names, ", ") + ")"
}

// expandSandboxValue turns one layer's sandbox value into a sandbox object: a template name
// into the template's fields, {"template": n, ...} into the template with the rest merged
// over it (mergeSandboxFields), an inline object into itself with the older key spellings
// renamed. null gives a nil object and reset true: no sandbox below it counts.
func expandSandboxValue(raw json.RawMessage, templates sandboxTemplates, where string) (fields map[string]json.RawMessage, reset bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, nil
	}
	if string(trimmed) == "null" {
		return nil, true, nil
	}
	var name string
	if json.Unmarshal(trimmed, &name) == nil {
		if strings.TrimSpace(name) == "" {
			return nil, false, configError("%s: sandbox must be an object or the name of a sandbox template", where)
		}
		fields, err := templates.read(name, where)
		if err != nil {
			return nil, false, err
		}
		fields["template"] = mustJSON(name)
		return fields, false, nil
	}
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, false, configError("%s: sandbox must be an object or the name of a sandbox template", where)
	}
	if err := canonicalSandboxSpelling(fields, where); err != nil {
		return nil, false, err
	}
	rawName, named := fields["template"]
	if !named {
		return fields, false, nil
	}
	if err := json.Unmarshal(rawName, &name); err != nil || strings.TrimSpace(name) == "" {
		return nil, false, configError("%s: sandbox.template must be the name of a sandbox template", where)
	}
	base, err := templates.read(name, where)
	if err != nil {
		return nil, false, err
	}
	delete(fields, "template")
	merged := mergeSandboxFields(base, fields)
	merged["template"] = mustJSON(name)
	return merged, false, nil
}

// canonicalSandboxSpelling renames the older writable_paths and plan_writable_paths to
// write.paths and write.plan_paths, so layers that use different spellings merge; one object
// with both spellings of one list is refused (D11).
func canonicalSandboxSpelling(fields map[string]json.RawMessage, where string) error {
	pairs := [][2]string{{"writable_paths", "paths"}, {"plan_writable_paths", "plan_paths"}}
	var write map[string]json.RawMessage
	for _, pair := range pairs {
		raw, has := fields[pair[0]]
		if !has {
			continue
		}
		if write == nil {
			write = map[string]json.RawMessage{}
			if existing, ok := fields["write"]; ok && json.Unmarshal(existing, &write) != nil {
				// Not an object: parseSandbox names the problem.
				return nil
			}
			if write == nil {
				write = map[string]json.RawMessage{}
			}
		}
		if _, both := write[pair[1]]; both {
			return configError("%s: sandbox.%s is the older spelling of sandbox.write.%s; set one of them", where, pair[0], pair[1])
		}
		write[pair[1]] = raw
		delete(fields, pair[0])
	}
	if write != nil {
		fields["write"] = mustJSON(write)
	}
	return nil
}

// mergeSandboxFields merges over onto base like settings layers (objects per key, values
// replace), except the lists that have an add_ key: an add_ list is added to the one below it,
// and a layer that sets a list clears the add_ lists below it, so its own add_ list and those
// of the layers above add to what it set (D26). isolation, read and write merge per key.
func mergeSandboxFields(base, over map[string]json.RawMessage) map[string]json.RawMessage {
	merged := make(map[string]json.RawMessage, len(base)+len(over))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range over {
		switch key {
		case "read", "write":
			merged[key] = mergeSandboxListObject(merged[key], value)
		case "isolation":
			merged[key] = mergeSettingValue(merged[key], value)
		default:
			mergeSandboxList(merged, key, value)
		}
	}
	return merged
}

// mergeSandboxListObject merges the read or write objects of two layers.
func mergeSandboxListObject(base, over json.RawMessage) json.RawMessage {
	var baseObject, overObject map[string]json.RawMessage
	if json.Unmarshal(over, &overObject) != nil || overObject == nil {
		return over
	}
	if len(base) == 0 || json.Unmarshal(base, &baseObject) != nil || baseObject == nil {
		baseObject = map[string]json.RawMessage{}
	}
	for key, value := range overObject {
		mergeSandboxList(baseObject, key, value)
	}
	return mustJSON(baseObject)
}

// mergeSandboxList sets key of object to value: an add_ key is appended to what is there; a
// list with an add_ key clears that add_ list; anything else replaces.
func mergeSandboxList(object map[string]json.RawMessage, key string, value json.RawMessage) {
	if add, has := sandboxAddKeys[key]; has {
		object[key] = value
		delete(object, add)
		return
	}
	for _, add := range sandboxAddKeys {
		if key != add {
			continue
		}
		var below, above []json.RawMessage
		if existing, ok := object[key]; ok && json.Unmarshal(existing, &below) == nil && json.Unmarshal(value, &above) == nil && above != nil {
			object[key] = mustJSON(append(below, above...))
			return
		}
	}
	object[key] = value
}

// composeSandboxLayers expands the sandbox value of each layer (root defaults, session,
// profile, in that order) and merges them (mergeSandboxFields). absent is true when no layer
// names a sandbox; a null layer drops the layers below it.
func composeSandboxLayers(layers []json.RawMessage, wheres []string, templates sandboxTemplates) (json.RawMessage, bool, error) {
	var composed map[string]json.RawMessage
	present := false
	for index, raw := range layers {
		fields, reset, err := expandSandboxValue(raw, templates, wheres[index])
		if err != nil {
			return nil, false, err
		}
		if reset {
			composed, present = nil, true
			continue
		}
		if fields == nil {
			continue
		}
		present = true
		if composed == nil {
			composed = fields
			continue
		}
		// An inline layer over a template keeps the template's name; a layer that names
		// another template brings that name.
		composed = mergeSandboxFields(composed, fields)
	}
	if !present {
		return nil, true, nil
	}
	if composed == nil {
		return json.RawMessage("null"), false, nil
	}
	return mustJSON(composed), false, nil
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// sandboxUsers names what refers to template name, so it cannot be deleted from under them: the
// root defaults, the sessions of every root, the profiles, and the agents defined inline in a
// toolset. Agent folders are read with their session (agents_dir); one that names a deleted
// template stops that session from loading, with the template named.
func sandboxUsers(layout Layout, roots []Root, name string) ([]string, error) {
	var users []string
	if sandboxValueNames(layout.Defaults["sandbox"], name) {
		users = append(users, "the root setting's defaults")
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			sessionID, ok := SessionFileID(entry)
			if !ok {
				continue
			}
			var session layoutSessionFile
			if readLayoutFile(filepath.Join(root.Directory, entry.Name()), &session) == nil && sandboxValueNames(session.Settings["sandbox"], name) {
				users = append(users, "session "+root.ID+":"+sessionID)
			}
		}
	}
	if profiles, err := catalog.Scan("profile", layout.Profiles); err == nil {
		for _, entry := range profiles.Entries() {
			var profile layoutProfileFile
			if readLayoutFile(entry.Path, &profile) == nil && sandboxValueNames(profile.Settings["sandbox"], name) {
				users = append(users, "profile "+entry.ID())
			}
		}
	}
	if toolsets, err := catalog.Scan("toolset", layout.Tools); err == nil {
		for _, entry := range toolsets.Entries() {
			var toolset layoutToolsetFile
			if readLayoutFile(entry.Path, &toolset) != nil {
				continue
			}
			var subagent struct {
				Agents map[string]map[string]json.RawMessage `json:"agents"`
			}
			if json.Unmarshal(toolset.Native["subagent"], &subagent) != nil {
				continue
			}
			for _, agent := range slices.Sorted(maps.Keys(subagent.Agents)) {
				if sandboxValueNames(subagent.Agents[agent]["sandbox"], name) {
					users = append(users, "agent "+agent+" of toolset "+entry.ID())
				}
			}
		}
	}
	return users, nil
}

// sandboxValueNames reports whether a sandbox value names template name, as a string or as an
// object's template.
func sandboxValueNames(raw json.RawMessage, name string) bool {
	if len(raw) == 0 {
		return false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == name
	}
	var object struct {
		Template string `json:"template"`
	}
	return json.Unmarshal(raw, &object) == nil && object.Template == name
}
