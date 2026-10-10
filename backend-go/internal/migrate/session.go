package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// handledKeys are the legacy top-level keys that move to a file of their own; every other
// key must be a setting the layout accepts, or it is reported as dropped.
var handledKeys = map[string]bool{
	"cwd": true, "paths": true, "mcp_servers": true, "tools": true, "prompt_documents": true,
	"prompt_role": true, "tool_definition_paths": true, "schema_version": true, "session_name": true,
}

// legacyProfile is one profile of a legacy session as the legacy loader sees it.
type legacyProfile struct {
	arg    string // the name to pass to the legacy loader; "" for a session without profiles
	name   string // the name shown
	config sessionfs.SessionConfig
	raw    map[string]json.RawMessage
	id     string // <category>/<name> in the new layout
}

// plan is everything read from one legacy session before anything is written.
type plan struct {
	root      sessionfs.Root
	name      string
	dir       string
	category  string
	location  sessionfs.Location
	setting   map[string]json.RawMessage
	cwd       string
	active    string
	profiles  []legacyProfile
	modelIDs  map[string]string // absolute legacy model file -> model ID
	modelList []string          // model files in first-use order
}

// fatalError marks a problem that stops the whole run, not only the session it came from.
type fatalError struct{ error }

func (r *runState) convertSession(root sessionfs.Root, name string) error {
	dir := filepath.Join(root.Directory, name)
	data, err := os.ReadFile(filepath.Join(dir, "setting.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	location := sessionfs.Location{Reference: root.ID + ":" + name, SessionID: name, Root: root, Directory: dir}
	if location.UsesLayout() {
		r.report.Skipped = append(r.report.Skipped, fmt.Sprintf("%s:%s already has a session.json", root.ID, name))
		return nil
	}
	setting, err := rawObject(data)
	if err != nil {
		r.report.Skipped = append(r.report.Skipped, fmt.Sprintf("%s:%s: setting.json: %v", root.ID, name, err))
		return nil
	}
	if other, clash := r.sessionNames[name]; clash {
		r.report.Skipped = append(r.report.Skipped, fmt.Sprintf("%s:%s: the session name is also used by root %s, and the layout keeps one state folder per session name", root.ID, name, other))
		return nil
	}
	p := &plan{root: root, name: name, dir: dir, category: identifier(name), location: location, setting: setting, modelIDs: map[string]string{}}
	if err := r.read(p); err != nil {
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return fatal.error
		}
		r.report.Skipped = append(r.report.Skipped, fmt.Sprintf("%s:%s: %v", root.ID, name, err))
		return nil
	}
	r.sessionNames[name] = root.ID
	return r.write(p)
}

// read loads the legacy session and each of its profiles. It writes nothing.
func (r *runState) read(p *plan) error {
	info, err := sessionfs.ReadPathProfileInfo(filepath.Join(p.dir, "setting.json"))
	if err != nil {
		return err
	}
	args := info.PathProfiles
	if len(args) == 0 {
		return errors.New("it has no paths.profiles, so its tool state (tasks, memory, fetch) lives under the cwd and would be left behind; add one profile to its setting.json and migrate again")
	}
	paths, _ := objectOf(p.setting["paths"])
	profiles, _ := objectOf(paths["profiles"])
	used := map[string]string{}
	for _, arg := range args {
		config, err := sessionfs.LoadSessionSettings(p.location, arg, "")
		if err != nil {
			return fmt.Errorf("profile %q does not load: %w", arg, err)
		}
		shown := arg
		if shown == "" {
			shown = "default"
		}
		item := legacyProfile{arg: arg, name: shown, config: config, id: p.category + "/" + identifier(shown)}
		if other, clash := used[item.id]; clash {
			return fmt.Errorf("profiles %q and %q both become %s", other, shown, item.id)
		}
		used[item.id] = shown
		item.raw, _ = objectOf(profiles[arg])
		if folder := r.stateFolderHoldingOutput(config.StoragePaths); folder != "" {
			return &fatalError{fmt.Errorf("the output %s is inside %s, a folder the profile %q keeps state in; the copy would never end. Choose another --out", r.out, folder, shown)}
		}
		p.profiles = append(p.profiles, item)
	}
	if err := checkEnvFiles(p); err != nil {
		return err
	}
	p.active = info.ActiveProfile
	if p.active == "" {
		p.active = "default"
	}
	cwd := p.profiles[0].config.WorkingDir
	p.cwd = cwd
	return nil
}

// checkEnvFiles refuses a session whose env_file names a file outside the session folder:
// the layout keeps only the folder, so the file could not be carried over (and a shared
// file would land in another session's place).
func checkEnvFiles(p *plan) error {
	tools, _ := objectOf(p.setting["tools"])
	native, _ := objectOf(tools["native"])
	for tool, raw := range native {
		fields, _ := objectOf(raw)
		var name string
		if json.Unmarshal(fields["env_file"], &name) != nil {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || filepath.IsAbs(expandHome(name)) {
			continue
		}
		relative := filepath.Clean(name)
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tools.native.%s.env_file %q is outside the session folder; move the file into the folder and migrate again", tool, name)
		}
	}
	return nil
}

// relocateEnvFiles rewrites each relative env_file of the tools. In the legacy session it
// was relative to the session folder; in the layout it is relative to the root folder, and
// copyEnvFiles puts the file in the session's folder of the new tree, so the path becomes
// "<sessions folder>/<session>/<file>". It stays relative, so the tree can be moved.
func (r *runState) relocateEnvFiles(p *plan, native map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(native))
	for tool, raw := range native {
		out[tool] = raw
		fields, ok := objectOf(raw)
		if !ok {
			continue
		}
		var name string
		if json.Unmarshal(fields["env_file"], &name) != nil {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || filepath.IsAbs(expandHome(name)) {
			continue
		}
		relative, err := filepath.Rel(r.out, filepath.Join(r.sessionsDir(p.root), p.name, name))
		if err != nil {
			continue
		}
		encoded, err := json.Marshal(filepath.ToSlash(relative))
		if err != nil {
			continue
		}
		copied := make(map[string]json.RawMessage, len(fields))
		for key, value := range fields {
			copied[key] = value
		}
		copied["env_file"] = encoded
		if rewritten, err := json.Marshal(copied); err == nil {
			out[tool] = rewritten
		}
	}
	return out
}

// absolutePathRules returns the tools.native map with every relative path rule made
// absolute. A relative rule is relative to the root folder, which is another folder in
// the new tree, so it would allow or deny another place.
func absolutePathRules(native map[string]json.RawMessage, base string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(native))
	for tool, raw := range native {
		out[tool] = raw
		fields, ok := objectOf(raw)
		paths, has := fields["paths"]
		if !ok || !has {
			continue
		}
		var value any
		if json.Unmarshal(paths, &value) != nil {
			continue
		}
		rewritten, err := json.Marshal(absoluteRules(value, base, false))
		if err != nil {
			continue
		}
		copied := make(map[string]json.RawMessage, len(fields))
		for key, item := range fields {
			copied[key] = item
		}
		copied["paths"] = rewritten
		if encoded, err := json.Marshal(copied); err == nil {
			out[tool] = encoded
		}
	}
	return out
}

// absoluteRules walks one paths value: an array of {action, path}, or {allow, deny} whose
// lists hold strings or {path} objects.
func absoluteRules(value any, base string, inside bool) any {
	switch item := value.(type) {
	case string:
		if !inside {
			return item
		}
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			return item
		}
		return resolveFrom(base, trimmed)
	case []any:
		out := make([]any, len(item))
		for index, entry := range item {
			out[index] = absoluteRules(entry, base, inside)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, entry := range item {
			switch key {
			case "allow", "deny", "path":
				out[key] = absoluteRules(entry, base, true)
			default:
				out[key] = absoluteRules(entry, base, inside)
			}
		}
		return out
	}
	return value
}

func objectOf(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return map[string]json.RawMessage{}, false
	}
	return fields, true
}

func (r *runState) write(p *plan) error {
	unlock, err := r.lockProfiles(p)
	if err != nil {
		return err
	}
	defer unlock()

	session := Session{Root: p.root.ID, Name: p.name}
	toolset, err := r.writeToolset(p)
	if err != nil {
		return err
	}
	for index := range p.profiles {
		profile := &p.profiles[index]
		if err := r.writeProfile(p, profile, toolset); err != nil {
			return err
		}
		files, moved, err := r.copyState(p, profile)
		if err != nil {
			return fmt.Errorf("copy the state of %s: %w", profile.name, err)
		}
		session.Profiles = append(session.Profiles, Profile{Legacy: profile.name, ID: profile.id, Files: files})
		r.converted = append(r.converted, convertedProfile{location: p.location, legacy: profile.arg, id: profile.id, session: p.name, root: p.root.ID, config: profile.config, moved: moved})
	}
	if err := r.copyModels(p); err != nil {
		return err
	}
	if err := r.copyEnvFiles(p); err != nil {
		return err
	}
	if err := r.writeSession(p); err != nil {
		return err
	}
	r.pathMap[p.dir] = filepath.Join(r.sessionsDir(p.root), p.name)
	r.noteToolDefinitions(p)
	r.report.Sessions = append(r.report.Sessions, session)
	return nil
}

func (r *runState) lockProfiles(p *plan) (func(), error) {
	var unlocks []func()
	release := func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}
	seen := map[string]bool{}
	for _, profile := range p.profiles {
		path := profile.config.StoragePaths.Messages
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			continue
		}
		unlock, err := r.lockProfile(path)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return release, nil
}

// modelID registers a legacy model file and returns its ID, or "" when the file is
// missing (the legacy session could not run with it either).
func (r *runState) modelID(p *plan, path string) string {
	if id, ok := p.modelIDs[path]; ok {
		return id
	}
	if _, err := os.Stat(path); err != nil {
		r.warn("%s:%s: model file %s is missing; it is left out", p.root.ID, p.name, path)
		p.modelIDs[path] = ""
		return ""
	}
	relative, err := filepath.Rel(p.dir, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		relative = filepath.Base(path)
	}
	name := identifier(strings.ReplaceAll(strings.TrimSuffix(relative, ".json"), string(filepath.Separator), "-"))
	id := p.category + "/" + name
	for suffix := 2; r.taken[r.modelPath(id)] != ""; suffix++ {
		id = fmt.Sprintf("%s/%s-%d", p.category, name, suffix)
	}
	r.taken[r.modelPath(id)] = path
	p.modelIDs[path] = id
	p.modelList = append(p.modelList, path)
	r.pathMap[path] = r.modelPath(id)
	return id
}

func (r *runState) modelPath(id string) string { return filepath.Join(r.out, "models", id+".json") }

func (r *runState) copyModels(p *plan) error {
	for _, path := range p.modelList {
		if _, err := r.copy(path, r.modelPath(p.modelIDs[path])); err != nil {
			return fmt.Errorf("copy model %s: %w", path, err)
		}
	}
	return nil
}

func (r *runState) writeProfile(p *plan, profile *legacyProfile, toolset string) error {
	config := profile.config
	files := config.Models
	if len(files) == 0 {
		files = []string{config.ModelPath}
	}
	var models []string
	for _, file := range files {
		if id := r.modelID(p, resolveFrom(p.dir, file)); id != "" {
			models = append(models, id)
		}
	}
	defaultID := r.modelID(p, config.ModelPath)
	if defaultID != "" && !contains(models, defaultID) {
		models = append([]string{defaultID}, models...)
	}
	if len(models) == 0 {
		r.warn("%s:%s: profile %s has no usable model, so it can use every model of the catalog", p.root.ID, p.name, profile.name)
	}
	file := map[string]any{
		"schema_version": 1,
		"description":    fmt.Sprintf("Migrated from %s, profile %s.", filepath.Join(p.dir, "setting.json"), profile.name),
		"toolset":        toolset,
	}
	if len(models) > 0 {
		file["models"] = models
		if defaultID != "" {
			file["model_default"] = defaultID
		}
	}
	if documents := promptDocuments(p.setting, profile.raw); len(documents) > 0 {
		file["prompt_documents"] = documents
	}
	if raw, ok := p.setting["prompt_role"]; ok {
		var role string
		if json.Unmarshal(raw, &role) == nil && role != "" {
			file["prompt_role"] = role
		}
	}
	if raw, ok := profile.raw["tools"]; ok {
		file["tools"] = r.profileTools(raw, p.root.GlobalJadRoot)
	}
	path := filepath.Join(r.out, "profiles", profile.id+".json")
	r.taken[path] = filepath.Join(p.dir, "setting.json")
	return writeJSON(path, file)
}

// profileTools makes the relative path rules of a profile's own tools block absolute.
func (r *runState) profileTools(raw json.RawMessage, base string) json.RawMessage {
	fields, ok := objectOf(raw)
	if !ok {
		return raw
	}
	native, ok := objectOf(fields["native"])
	if !ok {
		return raw
	}
	encoded, err := json.Marshal(absolutePathRules(native, base))
	if err != nil {
		return raw
	}
	fields["native"] = encoded
	if out, err := json.Marshal(fields); err == nil {
		return out
	}
	return raw
}

// promptDocuments is the session's documents followed by the profile's, the order the
// legacy loader joins them in.
func promptDocuments(setting, profile map[string]json.RawMessage) []string {
	var documents []string
	for _, source := range []map[string]json.RawMessage{setting, profile} {
		var items []string
		if raw, ok := source["prompt_documents"]; ok && json.Unmarshal(raw, &items) == nil {
			documents = append(documents, items...)
		}
	}
	return documents
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// writeToolset writes the session's tools.native and mcp_servers as one toolset and
// returns its ID.
func (r *runState) writeToolset(p *plan) (string, error) {
	id := p.category + "/main"
	file := map[string]any{
		"schema_version": 1,
		"description":    fmt.Sprintf("Migrated from the tools of %s.", filepath.Join(p.dir, "setting.json")),
	}
	tools, _ := objectOf(p.setting["tools"])
	for key := range tools {
		if key != "native" {
			r.warn("%s:%s: tools.%s has no place in a toolset and is left out", p.root.ID, p.name, key)
		}
	}
	if native, ok := objectOf(tools["native"]); ok {
		file["native"] = r.relocateEnvFiles(p, absolutePathRules(native, p.root.GlobalJadRoot))
	}
	servers, err := r.mcpServers(p)
	if err != nil {
		return "", err
	}
	if len(servers) > 0 {
		encoded, err := marshalOrdered(servers)
		if err != nil {
			return "", err
		}
		file["mcp"] = json.RawMessage(encoded)
	}
	path := filepath.Join(r.out, "tools", id+".json")
	r.taken[path] = filepath.Join(p.dir, "setting.json")
	return id, writeJSON(path, file)
}

// mcpServers copies each server's config file into mcp/<category>/<id>.json and returns the
// toolset entries in the legacy order. Servers with one id and different files get
// different categories, which is what the layout is for.
func (r *runState) mcpServers(p *plan) ([]pair, error) {
	var entries []pair
	for _, server := range p.profiles[0].config.MCPServers {
		if err := catalog.ValidateIdentifier(server.ID); err != nil {
			r.warn("%s:%s: MCP server id %s: %v; it is left out", p.root.ID, p.name, server.ID, err)
			continue
		}
		content, err := os.ReadFile(server.ConfigPath)
		if err != nil {
			r.warn("%s:%s: MCP server %s: %v; it is left out", p.root.ID, p.name, server.ID, err)
			continue
		}
		category := r.mcpCategory(p, server.ID, content)
		destination := filepath.Join(r.out, "mcp", category, server.ID+".json")
		if _, err := os.Stat(destination); os.IsNotExist(err) {
			if err := writeNew(destination, content); err != nil {
				return nil, err
			}
		}
		use := map[string]any{"enabled": server.Enabled}
		if server.DenyTools != nil {
			use["deny_tools"] = server.DenyTools
		}
		if server.Approval != "" {
			// off is written too: under approval.default "ask" the legacy loader keeps an
			// explicit off, and an absent key in the toolset would become ask.
			use["approval"] = server.Approval
		}
		entries = append(entries, pair{Key: category + "/" + server.ID, Value: use})
	}
	return entries, nil
}

func (r *runState) mcpCategory(p *plan, id string, content []byte) string {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	for _, seen := range r.mcpSeen[id] {
		if seen.hash == hash {
			return seen.category
		}
	}
	category := "shared"
	if len(r.mcpSeen[id]) > 0 {
		category = p.category
		for suffix := 2; categoryUsed(r.mcpSeen[id], category); suffix++ {
			category = fmt.Sprintf("%s-%d", p.category, suffix)
		}
	}
	r.mcpSeen[id] = append(r.mcpSeen[id], mcpCopy{hash: hash, category: category})
	return category
}

func categoryUsed(copies []mcpCopy, category string) bool {
	for _, item := range copies {
		if item.category == category {
			return true
		}
	}
	return false
}

func (r *runState) writeSession(p *plan) error {
	file := map[string]any{
		"schema_version": 1,
		"description":    fmt.Sprintf("Migrated from %s.", filepath.Join(p.dir, "setting.json")),
		"cwd":            p.cwd,
		"name":           p.profiles[0].config.SessionName,
		"scope":          map[string]any{"profiles": []string{p.category}},
		"active_profile": p.category + "/" + identifier(p.active),
	}
	settings := map[string]json.RawMessage{}
	var dropped []string
	for key, value := range p.setting {
		switch {
		case handledKeys[key]:
		case sessionfs.LayoutSettingKey(key):
			settings[key] = value
		default:
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	for _, key := range dropped {
		r.warn("%s:%s: setting %q has no place in the layout and is left out", p.root.ID, p.name, key)
	}
	if len(settings) > 0 {
		file["settings"] = settings
	}
	return writeJSON(filepath.Join(r.sessionsDir(p.root), p.name, "session.json"), file)
}

func (r *runState) sessionsDir(root sessionfs.Root) string {
	if len(r.source.Roots) == 1 {
		return filepath.Join(r.out, "sessions")
	}
	return filepath.Join(r.out, "sessions-"+root.ID)
}

// copyEnvFiles copies the env files the tools name by a relative path; the layout keeps
// the session folder, and the tools still resolve them against it.
func (r *runState) copyEnvFiles(p *plan) error {
	tools, _ := objectOf(p.setting["tools"])
	native, _ := objectOf(tools["native"])
	for tool, raw := range native {
		fields, _ := objectOf(raw)
		var name string
		if json.Unmarshal(fields["env_file"], &name) != nil {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || filepath.IsAbs(expandHome(name)) {
			continue
		}
		source := filepath.Join(p.dir, name)
		if _, err := os.Stat(source); err != nil {
			r.warn("%s:%s: tools.native.%s.env_file %s does not exist", p.root.ID, p.name, tool, name)
			continue
		}
		destination := filepath.Join(r.sessionsDir(p.root), p.name, name)
		if _, err := os.Stat(destination); err == nil {
			continue
		}
		if _, err := r.copy(source, destination); err != nil {
			return err
		}
	}
	return nil
}

func (r *runState) noteToolDefinitions(p *plan) {
	list := p.profiles[0].config.ToolDefinitionPaths
	if len(r.toolDef) == 0 {
		r.toolDef = append([]string(nil), list...)
		return
	}
	for _, path := range list {
		if !contains(r.toolDef, path) {
			r.toolDef = append(r.toolDef, path)
			r.warn("%s:%s: tool_definition_paths differs from an earlier session; the root setting holds the union", p.root.ID, p.name)
		}
	}
}
