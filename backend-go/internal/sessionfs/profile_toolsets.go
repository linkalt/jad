package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"jad/backend-go/internal/catalog"
)

// A profile's own toolsets (phase 3c.3, docs/design_workspace_layout.md section 21, decider 3c
// C1 to C4): files in W/.jad/profile-own/<category>/<name>/tools/<toolset category>/<toolset
// name>.json. Such a file is not a whole toolset: it names a shared toolset with its scope
// (based_on), the tools and MCP servers of that toolset the profile leaves out (omit) and the
// few tuning keys of its tools it changes (set). Everything else, the hooks, the MCP
// connections, the paths, commands, environments, endpoints and limits of the tools, comes from
// the base, so it is never written in profile-own and follows every edit of the base. An own
// toolset is used by its profile's own runs only: never by a sub-agent, another profile, a
// profile_message caller or the catalog routes.

const profileToolsetsFolder = "tools"

// ownToolsetKeys are the keys of an own toolset file.
var ownToolsetKeys = []string{"schema_version", "based_on", "omit", "set"}

// ownToolsetOmitKeys are the parts of a base toolset an own toolset may leave out, and
// ownToolsetSetKeys the parts whose tools it may tune.
var (
	ownToolsetOmitKeys = []string{"native", "extension", "mcp"}
	ownToolsetSetKeys  = []string{"native", "extension"}
)

// The reasons a key of an own toolset's tool settings is refused by name. 3c.3 refuses each of
// them at every profile_self_edit level, also under full, and in files written by hand.
const (
	refusedToolReach    = "it changes what the tool can reach (other sessions, profiles, remotes, agents, tools, domains or types)"
	refusedToolPath     = "it names a file or folder the tool reads or writes"
	refusedToolCommand  = "it names a command, interpreter, runner or server the tool starts"
	refusedToolEnv      = "it changes the environment or the secrets a command gets"
	refusedToolLimit    = "it is a limit on what the tool may start, keep or store, which only the user changes"
	refusedToolEngine   = "it tunes how the engine runs the tool, not what the profile's runs see"
	refusedToolEndpoint = "the endpoint, the credential, the provider and the model come from the base toolset"
	refusedToolAccept   = "it decides when a draft is accepted without the user"
)

// ownScalar is a tuning value: a string, a number or a boolean. The tool's own rules (its
// normalizer) judge the composed value, its range included.
var ownScalar = ownKeyRule{kind: "scalar"}

// ownToolRule is the rule of one tool's settings in an own toolset's set: the tuning keys and
// the keys refused by name; every other key is refused, keys added to the tool later too.
func ownToolRule(tune []string, refused ...map[string]string) ownKeyRule {
	rule := ownKeyRule{kind: "object", keys: make(map[string]ownKeyRule, len(tune)), refused: map[string]string{}}
	for _, key := range tune {
		rule.keys[key] = ownScalar
	}
	for _, group := range refused {
		maps.Copy(rule.refused, group)
	}
	return rule
}

// refuseKeys is the refused part of a rule: each key with reason.
func refuseKeys(reason string, keys ...string) map[string]string {
	group := make(map[string]string, len(keys))
	for _, key := range keys {
		group[key] = reason
	}
	return group
}

// bashStyleRefused are the refused keys the shell-style tools (exec, tool_smith) share, and
// paths too.
func bashStyleRefused(paths ...string) []map[string]string {
	return []map[string]string{
		refuseKeys(refusedToolCommand, "shell", "login_shell"),
		refuseKeys(refusedToolLimit, "max_timeout_ms", "standby_max", "standby_pool"),
		refuseKeys(refusedToolEngine, "kill_grace_ms", "kill_deadline_ms", "drain_after_exit_ms", "kill_process_group", "kill_children_on_run_end", "standby_idle_ms"),
		refuseKeys(refusedToolPath, append([]string{"stdout", "stderr", "spill", "functions_dir"}, paths...)...),
		refuseKeys(refusedToolEnv, "env_file", "env", "env_deny"),
	}
}

// ownToolsetTools is the classification of every key of every native and extension tool for
// an own toolset's set (decider 3c C1): tuning keys an own toolset may change, and every other
// key refused with its reason. TestOwnToolsetTableClassifiesEveryToolKey keeps it complete
// against the tools' key lists.
var ownToolsetTools = map[string]map[string]ownKeyRule{
	"native": {
		"async": ownToolRule([]string{"default_block_ms", "default_wait_ms", "wait_max_signals", "list_default_limit", "list_max_limit", "summary_max_bytes", "status_summary_max_bytes", "response_max_bytes", "latest_bytes", "latest_lines", "latest_value_bytes"},
			refuseKeys(refusedToolLimit, "max_handles", "max_handles_total", "max_block_ms", "max_wait_ms", "keep_finished", "retention_days", "unread_max", "unread_ttl_days", "latest_total_bytes", "latest_scan_max_bytes"),
			refuseKeys(refusedToolEngine, "stop_wait_ms", "sse_max_ids", "index_flush_ms", "peer_check_ms")),
		"exec": ownToolRule([]string{"timeout_ms", "max_bytes", "max_line_chars", "stdin_max_bytes", "strip_ansi", "arg_max_bytes", "code_max_bytes", "default_lang", "function_max_bytes", "input_max_bytes", "line_aware", "list_header_bytes", "list_max_files", "list_max_results", "result_max_bytes"},
			append(bashStyleRefused("paths"), refuseKeys(refusedToolCommand, "runners"))...),
		"tool_smith": ownToolRule([]string{"timeout_ms", "max_bytes", "max_line_chars", "stdin_max_bytes", "strip_ansi", "code_max_bytes", "result_max_bytes", "nested_result_max_bytes", "nested_request_max_bytes", "list_max_results", "list_max_files", "list_header_bytes", "function_max_bytes"},
			append(bashStyleRefused(), refuseKeys(refusedToolCommand, "interpreter"), refuseKeys(refusedToolLimit, "max_nested_calls", "max_parallel_calls"), refuseKeys(refusedToolReach, "tools"))...),
		"code_context": ownToolRule([]string{"max_description_chars", "max_range_lines", "max_file_bytes", "max_output_bytes", "default_priority", "max_find_results", "max_batch_items", "relocate"},
			refuseKeys(refusedToolPath, "storage_dir", "paths"),
			refuseKeys(refusedToolLimit, "max_entries", "max_storage_bytes", "max_scan_tasks")),
		"lsp": ownToolRule([]string{"request_timeout_ms", "diagnostics_wait_ms", "max_results", "max_file_bytes"},
			refuseKeys(refusedToolCommand, "servers"), refuseKeys(refusedToolPath, "paths"), refuseKeys(refusedToolEngine, "idle_shutdown_ms")),
		"read_image": ownToolRule([]string{"max_image_bytes"}, refuseKeys(refusedToolPath, "paths")),
		"skill":      ownToolRule([]string{"max_bytes"}),
		"load_tool":  ownToolRule(nil),
		"profile": ownToolRule([]string{"max_entries", "scan_max_entries"},
			refuseKeys(refusedToolReach, "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_remote", "remotes")),
		"profile_message": ownToolRule([]string{"timeout_ms", "busy_wait_ms", "result_max_bytes", "default_block_ms"},
			refuseKeys(refusedToolReach, "deny", "context_trigger", "remotes", "model_choice", "sandbox_choice"),
			refuseKeys(refusedToolLimit, "max_chain", "max_queued_per_profile")),
		"subagent": ownToolRule([]string{"result_max_bytes", "list_max_bytes", "changed_files_max", "default_block_ms", "remote_timeout_ms"},
			refuseKeys(refusedToolReach, "agents", "deny_agents", "background_writers", "statecontext", "remotes", "model_choice", "sandbox_choice"),
			refuseKeys(refusedToolPath, "agents_dir", "agents_dir_missing_ok", "agents_dir_override", "storage_dir", "worktree"),
			refuseKeys(refusedToolLimit, "max_concurrent", "keep_finished", "max_queued", "persist_runs", "delete_transcripts", "max_background_writers", "max_depth", "max_runs_per_root", "max_runs_per_session")),
		"task": ownToolRule([]string{"list_limit", "max_list_limit", "max_list_bytes", "max_batch_ops", "max_title_chars", "max_tags", "max_tag_chars", "max_documents", "max_document_chars"},
			refuseKeys(refusedToolPath, "storage_dir"), refuseKeys(refusedToolReach, "statecontext"), refuseKeys(refusedToolLimit, "max_tasks", "max_storage_bytes")),
		"records": ownToolRule([]string{"list_limit", "max_list_limit", "max_list_bytes", "max_scan_records", "max_scan_bytes", "max_filter_fields", "max_filter_depth", "max_json_depth", "cursor_ttl_ms", "max_cursor_bytes", "max_query_bytes"},
			refuseKeys(refusedToolPath, "storage_dir"), refuseKeys(refusedToolReach, "statecontext", "allowed_types"),
			refuseKeys(refusedToolLimit, "max_types", "max_records_per_type", "max_total_records", "max_record_bytes", "max_total_bytes", "max_cursors")),
		"request_draft": ownToolRule([]string{"timeout_ms", "extend_ms", "max_draft_bytes", "max_draft_depth", "max_title_chars"},
			refuseKeys(refusedToolLimit, "max_timeout_ms"), refuseKeys(refusedToolAccept, "headless_auto_accept_timeout_ms")),
		"transcript": ownToolRule([]string{"response_max_bytes", "line_max_bytes", "scan_max_bytes", "suggest_max", "timeout_ms", "preview_bytes", "value_bytes", "max_list_entries", "max_detail_bytes"},
			refuseKeys(refusedToolReach, "allow_other_sessions", "allow_other_reasoning", "allow_other_profiles", "allow_children", "allow_path", "allow_remote", "remotes"),
			refuseKeys(refusedToolEngine, "index_cache_entries")),
		"transcripts": ownToolRule([]string{"max_entries", "scan_max_entries", "timeout_ms"},
			refuseKeys(refusedToolReach, "allow_other_sessions", "allow_other_profiles", "allow_children", "allow_remote", "remotes")),
	},
	"extension": {
		"decide_systemone": ownToolRule([]string{"timeout_ms", "max_questions", "max_input_bytes", "max_response_bytes"},
			refuseKeys(refusedToolEndpoint, "base_url", "path", "api_key_env", "model")),
		"web_search": ownToolRule([]string{"default_results", "max_results", "timeout_ms", "max_response_bytes", "max_snippet_chars", "max_query_chars"},
			refuseKeys(refusedToolEndpoint, "provider", "base_url", "api_key_env", "user_agent"),
			refuseKeys(refusedToolReach, "allowed_domains", "blocked_domains")),
	},
}

// ownToolsetFile is an own toolset file as read.
type ownToolsetFile struct {
	BasedOn string
	// Omit are the names the profile leaves out of its base, by part (native, extension, mcp).
	Omit map[string][]string
	// Set are the tuning keys of each tool, by part (native, extension) and tool.
	Set map[string]map[string]map[string]json.RawMessage
}

// parseOwnToolset applies the own toolset file rules to contents.
func parseOwnToolset(path string, contents []byte) (ownToolsetFile, error) {
	fields, err := parseSessionObject(path, contents)
	if err != nil {
		return ownToolsetFile{}, err
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(ownToolsetKeys, key) {
			return ownToolsetFile{}, configError("%s: %s is not a key of a profile's own toolset (keys: %s); every other value comes from the toolset it is based on", path, key, strings.Join(ownToolsetKeys, ", "))
		}
	}
	if err := validateSchemaVersion(fields, path); err != nil {
		return ownToolsetFile{}, err
	}
	var file ownToolsetFile
	if err := json.Unmarshal(fields["based_on"], &file.BasedOn); err != nil || file.BasedOn == "" {
		return ownToolsetFile{}, configError("%s: based_on must name the toolset it is based on, with its scope, for example global:coding/full", path)
	}
	scope, _, _ := strings.Cut(file.BasedOn, ":")
	if scope != catalog.ScopeGlobal && scope != catalog.ScopeWorkspace {
		return ownToolsetFile{}, configError("%s: based_on %q must start with global: or workspace:; a profile's own toolset is based on a shared one, never on another own toolset", path, file.BasedOn)
	}
	if raw, ok := fields["omit"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if file.Omit, err = parseOwnToolsetOmit(path, raw); err != nil {
			return ownToolsetFile{}, err
		}
	}
	if raw, ok := fields["set"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if file.Set, err = parseOwnToolsetSet(path, raw); err != nil {
			return ownToolsetFile{}, err
		}
	}
	return file, nil
}

// parseOwnToolsetOmit reads omit: for each part, a list of distinct names.
func parseOwnToolsetOmit(path string, raw json.RawMessage) (map[string][]string, error) {
	var parts map[string]json.RawMessage
	if !jsonKindIs(bytes.TrimSpace(raw), "object") || json.Unmarshal(raw, &parts) != nil {
		return nil, configError("%s: omit must be a JSON object", path)
	}
	omit := make(map[string][]string, len(parts))
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		if !slices.Contains(ownToolsetOmitKeys, part) {
			return nil, configError("%s: omit.%s is not a part an own toolset leaves out (parts: %s); the base's hooks always apply", path, part, strings.Join(ownToolsetOmitKeys, ", "))
		}
		var names []string
		if err := json.Unmarshal(parts[part], &names); err != nil || names == nil {
			return nil, configError("%s: omit.%s must be an array of names", path, part)
		}
		for index, name := range names {
			if strings.TrimSpace(name) == "" || slices.Contains(names[:index], name) {
				return nil, configError("%s: omit.%s[%d] must be a name, given once", path, part, index)
			}
		}
		omit[part] = names
	}
	return omit, nil
}

// parseOwnToolsetSet reads set: for each part, the tools and the tuning keys of each, checked
// against ownToolsetTools.
func parseOwnToolsetSet(path string, raw json.RawMessage) (map[string]map[string]map[string]json.RawMessage, error) {
	var parts map[string]json.RawMessage
	if !jsonKindIs(bytes.TrimSpace(raw), "object") || json.Unmarshal(raw, &parts) != nil {
		return nil, configError("%s: set must be a JSON object", path)
	}
	set := make(map[string]map[string]map[string]json.RawMessage, len(parts))
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		rules, known := ownToolsetTools[part]
		if !known {
			return nil, configError("%s: set.%s cannot be changed by a profile's own toolset (parts: %s); its MCP servers and hooks come from the base", path, part, strings.Join(ownToolsetSetKeys, ", "))
		}
		var tools map[string]json.RawMessage
		if !jsonKindIs(bytes.TrimSpace(parts[part]), "object") || json.Unmarshal(parts[part], &tools) != nil {
			return nil, configError("%s: set.%s must be a JSON object", path, part)
		}
		set[part] = make(map[string]map[string]json.RawMessage, len(tools))
		for _, tool := range slices.Sorted(maps.Keys(tools)) {
			rule, known := rules[tool]
			if !known {
				return nil, configError("%s: set.%s.%s is not a %s tool (tools: %s)", path, part, tool, part, strings.Join(slices.Sorted(maps.Keys(rules)), ", "))
			}
			if err := rule.checkAs("own toolset", path, "set."+part+"."+tool, tools[tool]); err != nil {
				return nil, err
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(tools[tool], &keys); err != nil {
				return nil, configError("%s: set.%s.%s must be a JSON object", path, part, tool)
			}
			set[part][tool] = keys
		}
	}
	return set, nil
}

// composeOwnToolset is the toolset an own toolset file makes of its base (a whole toolset file,
// with a global one's relative paths already made absolute from G): the parts it omits left out,
// each tool it sets merged key by key with the base's settings of that tool. An own toolset adds
// no tool, server or hook. strict (an edit's trial, jad check) refuses a name it omits or sets
// that the base does not have; a load (not strict) passes over such a name, so an edit of the
// base that drops a tool or server does not take the profile out, and still nothing is added.
func composeOwnToolset(path string, file ownToolsetFile, base layoutToolsetFile, strict bool) (layoutToolsetFile, error) {
	composed := base
	composed.Native = maps.Clone(base.Native)
	composed.Extension = maps.Clone(base.Extension)
	for _, part := range ownToolsetSetKeys {
		tools := composed.Native
		if part == "extension" {
			tools = composed.Extension
		}
		for _, name := range file.Omit[part] {
			if _, found := tools[name]; !found && strict {
				return layoutToolsetFile{}, configError("%s: omit.%s: %s is not a %s tool of its base %s", path, part, name, part, file.BasedOn)
			}
			delete(tools, name)
		}
		for _, name := range slices.Sorted(maps.Keys(file.Set[part])) {
			raw, found := tools[name]
			if !found && !strict {
				continue
			}
			if !found {
				return layoutToolsetFile{}, configError("%s: set.%s.%s: its base %s has no %s tool %s (left out or never there); an own toolset adds no tool", path, part, name, file.BasedOn, part, name)
			}
			var settings map[string]json.RawMessage
			if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
				return layoutToolsetFile{}, configError("%s: set.%s.%s: its base %s gives %s.%s as no object", path, part, name, file.BasedOn, part, name)
			}
			merged, err := json.Marshal(mergeSettings(settings, file.Set[part][name]))
			if err != nil {
				return layoutToolsetFile{}, err
			}
			tools[name] = merged
		}
	}
	if omitted := file.Omit["mcp"]; len(omitted) > 0 {
		mcp, err := omitMCPServers(base.MCP, omitted, strict)
		if err != nil {
			return layoutToolsetFile{}, configError("%s: omit.mcp: %v (its base %s)", path, err, file.BasedOn)
		}
		composed.MCP = mcp
	}
	return composed, nil
}

// omitMCPServers is the base's mcp object without the servers named, in the base's order; strict
// refuses a name the base does not have.
func omitMCPServers(mcp json.RawMessage, omitted []string, strict bool) (json.RawMessage, error) {
	if len(bytes.TrimSpace(mcp)) == 0 || string(bytes.TrimSpace(mcp)) == "null" {
		if strict {
			return nil, fmt.Errorf("%s is not an MCP server of the base", omitted[0])
		}
		return mcp, nil
	}
	order, err := orderedObjectKeys(mcp)
	if err != nil {
		return nil, fmt.Errorf("the base's mcp is not an object")
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(mcp, &servers); err != nil {
		return nil, err
	}
	for _, name := range omitted {
		if _, found := servers[name]; !found {
			if strict {
				return nil, fmt.Errorf("%s is not an MCP server of the base", name)
			}
			continue
		}
		delete(servers, name)
		order = slices.DeleteFunc(order, func(key string) bool { return key == name })
	}
	if len(servers) == 0 {
		return nil, nil
	}
	return marshalOrderedObject(servers, order)
}

// ownToolsetBase is the shared toolset one of a profile's own toolsets is based on.
type ownToolsetBase struct {
	// BasedOn is the reference the own file names, with its scope prefix.
	BasedOn string
	// Path is the base's toolset file.
	Path string
	file ownToolsetFile
}

// ownToolsets is the toolset catalog of one profile's load: the shared catalog with the
// profile's own toolsets as the "profile" scope (decider 3c C2).
type ownToolsets struct {
	layout Layout
	// shared is the shared catalog alone; catalog adds the profile scope to it.
	shared, catalog *catalog.Catalog
	// bases are the own toolset files whose base resolves, by file.
	bases map[string]ownToolsetBase
	// faults are the own toolset files that cannot be used, by file, and why (marked).
	faults map[string]error
	// ids are the IDs of the own toolset files, by file.
	ids map[string]string
	// notes are warnings about own toolset files left out for their names or a link.
	notes []string
}

// profileToolsets is the toolset catalog the profile id loads with: in the workspace layout the
// shared toolsets and its own; elsewhere, for no profile, and without own toolsets the shared
// catalog. An own file that fails stays in the catalog: a profile that names it fails like one
// whose toolset file does not parse (it is left out of the session with a warning).
func (l Layout) profileToolsets(scans *CatalogScans, id string) (ownToolsets, error) {
	scopes, err := scans.scanScopeList("toolset", l.Tools, l.WorkspaceTools)
	if err != nil {
		return ownToolsets{}, err
	}
	shared := catalog.MergeScopes("toolset", scopes...)
	own := ownToolsets{layout: l, shared: shared, catalog: shared}
	if l.Workspace == "" || id == "" || l.ProfileOwnDir(id) == "" {
		return own, nil
	}
	overlay := scans.ownFileOverlay(ownToolsetKind, id)
	dir, err := ownFolder(l.Workspace, id, []string{profileToolsetsFolder}, false)
	if err == nil && dir == "" && len(overlay) > 0 {
		// A trial of an edit that makes the first own toolset: the folder is made when the edit
		// is published.
		dir, err = ownKindFolder(l.Workspace, id, profileToolsetsFolder)
	}
	if err != nil {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: its own toolsets are left out: %v", id, err))
		return own, nil
	}
	if dir == "" {
		return own, nil
	}
	scanned, err := scans.Scan("toolset", dir)
	if err != nil {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: its own toolsets are left out: %v", id, err))
		return own, nil
	}
	for _, skipped := range scanned.Skipped() {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: own toolset left out: %s", id, skipped))
	}
	for _, toolsetID := range slices.Sorted(maps.Keys(overlay)) {
		if overlay[toolsetID] == nil {
			scanned = scanned.Without(toolsetID)
			continue
		}
		category, name, _ := strings.Cut(toolsetID, "/")
		scanned = scanned.With(catalog.Entry{Category: category, Name: name, Path: filepath.Join(dir, category, name+".json")})
	}
	if scanned.Len() == 0 {
		return own, nil
	}
	own.bases, own.faults, own.ids = make(map[string]ownToolsetBase, scanned.Len()), map[string]error{}, make(map[string]string, scanned.Len())
	for _, entry := range scanned.Entries() {
		own.ids[entry.Path] = entry.ID()
		scans.noteFile(entry.Path)
		base, err := resolveOwnToolset(entry.Path, shared, overlay[entry.ID()])
		if err != nil {
			own.faults[entry.Path] = ownToolsetError(err)
			continue
		}
		scans.noteFile(base.Path)
		own.bases[entry.Path] = base
	}
	own.catalog = catalog.MergeScopes("toolset", append(scopes, catalog.Scope{Name: catalog.ScopeProfile, Catalog: scanned})...)
	for _, skipped := range own.catalog.Skipped() {
		if PathWithin(dir, skipped.Path) {
			own.notes = append(own.notes, fmt.Sprintf("profile %s: own toolset left out: %s", id, skipped))
		}
	}
	return own, nil
}

// resolveOwnToolset reads the own toolset file at path (or the trial's candidate) and finds its
// base among the shared toolsets.
func resolveOwnToolset(path string, shared *catalog.Catalog, trial *[]byte) (ownToolsetBase, error) {
	var contents []byte
	if trial != nil {
		contents = *trial
	} else {
		if info, err := os.Lstat(filepath.Dir(path)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ownToolsetBase{}, configError("%s: its category folder is a link or not a folder", path)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return ownToolsetBase{}, configError("%s: could not be read: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			return ownToolsetBase{}, configError("%s: a profile's own toolset must be a regular file, not a link", path)
		}
		if contents, err = readRegularFile(path, maxInternalJSONFileBytes); err != nil {
			return ownToolsetBase{}, configError("%s: could not be read: %v", path, err)
		}
	}
	file, err := parseOwnToolset(path, contents)
	if err != nil {
		return ownToolsetBase{}, err
	}
	base, err := shared.Lookup(file.BasedOn)
	if err != nil {
		return ownToolsetBase{}, configError("%s: based_on: %v", path, err)
	}
	return ownToolsetBase{BasedOn: file.BasedOn, Path: base.Path, file: file}, nil
}

// own reports whether path is one of the profile's own toolset files.
func (o ownToolsets) own(path string) bool {
	if _, found := o.bases[path]; found {
		return true
	}
	_, found := o.faults[path]
	return found
}

// toolset is the toolset file at path as the profile's load applies it: an own toolset composed
// with its base (a global base's relative paths made absolute from G first, so the own file's
// place changes nothing), a shared one read as it is. An own toolset's error is marked.
func (o ownToolsets) toolset(path string) (layoutToolsetFile, error) {
	return o.composed(path, false)
}

// strictToolset is toolset with the edit's and jad check's rule: an own toolset that omits or
// sets a name its base does not have fails (composeOwnToolset).
func (o ownToolsets) strictToolset(path string) (layoutToolsetFile, error) {
	return o.composed(path, true)
}

func (o ownToolsets) composed(path string, strict bool) (layoutToolsetFile, error) {
	if err, failed := o.faults[path]; failed {
		return layoutToolsetFile{}, err
	}
	base, own := o.bases[path]
	var file layoutToolsetFile
	if !own {
		err := readLayoutFile(path, &file)
		return file, err
	}
	if err := readLayoutFile(base.Path, &file); err != nil {
		return layoutToolsetFile{}, ownToolsetError(configError("%s: its base %s: %v", path, base.BasedOn, err))
	}
	if o.layout.globalToolset(base.Path) {
		file = absoluteGlobalToolsetPaths(file, o.layout.GlobalDir)
	}
	composed, err := composeOwnToolset(path, base.file, file, strict)
	return composed, ownToolsetError(err)
}

// problem is nil when the toolset file at path, own or shared, can be read and composed.
func (o ownToolsets) problem(path string) error {
	_, err := o.toolset(path)
	return err
}

// basedOn is the reference of the first own toolset (by file) based on the toolset file at
// path; "" when none is.
func (o ownToolsets) basedOn(path string) string {
	path = filepath.Clean(path)
	for _, file := range slices.Sorted(maps.Keys(o.bases)) {
		if filepath.Clean(o.bases[file].Path) == path {
			return catalog.ScopeProfile + ":" + o.ids[file]
		}
	}
	return ""
}

// OwnToolsetError marks an error about one of a profile's own toolset files (phase 3c.3), as
// OwnModelError marks one about an own model: its text names a file under profile-own and why it
// fails, which only the profile itself, its session and the server log show
// (app.otherProfileError).
type OwnToolsetError struct{ Err error }

func (e *OwnToolsetError) Error() string { return e.Err.Error() }
func (e *OwnToolsetError) Unwrap() error { return e.Err }

// IsOwnToolsetError reports whether err is, or wraps, an error about an own toolset file.
func IsOwnToolsetError(err error) bool {
	var own *OwnToolsetError
	return errors.As(err, &own)
}

// ownToolsetError marks err as an error about an own toolset file; nil stays nil.
func ownToolsetError(err error) error {
	if err == nil || IsOwnToolsetError(err) {
		return err
	}
	return &OwnToolsetError{Err: err}
}

// keepOwnToolsetMark is wrapped, marked when cause is (a ConfigError made with %v keeps no type).
func keepOwnToolsetMark(cause, wrapped error) error {
	if IsOwnToolsetError(cause) {
		return ownToolsetError(wrapped)
	}
	return wrapped
}
