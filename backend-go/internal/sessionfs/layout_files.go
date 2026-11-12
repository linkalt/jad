package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"jad/backend-go/internal/catalog"
)

// Layout is where the catalog layout keeps its files (sample/jad shows one). Every
// directory is absolute. Defaults holds the root setting `defaults`; ToolDefinitionPaths
// the root `libraries.tool_definition_paths`.
type Layout struct {
	Profiles string
	Models   string
	Tools    string
	MCP      string
	Remotes  string
	State    string
	// Sandbox is the folder of the sandbox templates (catalogs.sandbox) when it is a fixed
	// path; SandboxFromCWD is set instead when catalogs.sandbox starts with {cwd}: the rest of
	// the path, taken from each session's cwd (SandboxTemplateDir).
	Sandbox        string
	SandboxFromCWD string
	// RootSetting is the root setting file the layout was read from (the global setting in
	// the workspace layout; empty when there is none).
	RootSetting         string
	Defaults            map[string]json.RawMessage
	ToolDefinitionPaths []string

	// The fields below are set only by the workspace layout (v2, docs/design_workspace_layout.md);
	// they are empty for a layout read from a root setting.json, so v1 is unchanged.
	//
	// Workspace is the workspace directory W; WorkspaceSetting is W/.jad/workspace.json and
	// GlobalDir the global folder G. Models and Tools are the global folders,
	// WorkspaceModels and WorkspaceTools the workspace's private ones: a name is looked up in
	// both, and a name in both is an error (catalog.Merge).
	Workspace        string
	WorkspaceSetting string
	GlobalDir        string
	WorkspaceModels  string
	WorkspaceTools   string
	// SessionsDir is W/.jad/sessions; kept read-only in a sandbox with the other config folders.
	SessionsDir string
	// FixedHidePaths are hidden in every sandbox of a session of this workspace, after the
	// profile's hide_paths: G/auth and the .jad folder of every other listed workspace. A
	// profile or template can add hidden paths but cannot remove these.
	FixedHidePaths []string
	// SealedPaths are G and W/.jad, as written and with links resolved: read-only as a whole
	// in every sandbox of the workspace (SandboxSettings.SealedPaths). SealedHelpers are the
	// helper folders of exec and tool_smith inside W/.jad that stay writable to the tool that
	// owns them when the sandbox covers it.
	SealedPaths   []string
	SealedHelpers map[string]string
	// Libraries are the content libraries of the workspace (global and own folders).
	Libraries *Libraries
	// DefaultLayers are the layers Defaults was merged from, in order (built-in, global,
	// workspace), kept so profile config can say where each value comes from.
	DefaultLayers []SettingLayer
	// ProfileOwnRoots are the profile-own folders (<dir>/.jad/profile-own) of every known
	// workspace, this one too, as written: a prompt document that resolves into one of them,
	// after links, and not into the profile's own folder is a load error (design D1).
	ProfileOwnRoots []string
	// ProfileSelfEdit is the profile_self_edit level of the profiles of the workspace (off, on
	// or full): the lower of the level of global.json (on when unset) and of workspace.json.
	// While the global layout is read it holds the value of global.json ("" when unset).
	ProfileSelfEdit string

	// scope and scopeUsers are set by ScopedCatalogLayout for the catalog routes only.
	scope      catalogScope
	scopeUsers []scopeUser
}

// ScanModels is the model catalog of the layout: the models folder, and in the workspace
// layout the workspace's private models too (catalog.Merge).
func (l Layout) ScanModels(scans *CatalogScans) (*catalog.Catalog, error) {
	return scans.scanScopes("model", l.Models, l.WorkspaceModels)
}

// ScanToolsets is the toolset catalog of the layout, like ScanModels.
func (l Layout) ScanToolsets(scans *CatalogScans) (*catalog.Catalog, error) {
	return scans.scanScopes("toolset", l.Tools, l.WorkspaceTools)
}

// layoutSettingKeys are the setting keys a `settings` object (session, profile) and the
// root `defaults` may carry. Everything else has its own file or field in the new layout.
var layoutSettingKeys = map[string]struct{}{
	"statecontext": {}, "max_tool_rounds": {}, "max_parallel_tool_calls": {}, "tool_call_scheduling": {},
	"provider_retry": {}, "provider_stream": {}, "provider_network": {}, "max_run_tokens": {}, "max_run_cost_usd": {}, "max_run_duration_ms": {}, "model_fallback": {}, "checkpoint": {}, "plan_mode": {},
	"hooks": {}, "image_resize": {}, "image_store": {}, "persistence": {}, "front_compaction": {},
	"compaction": {}, "http_debug": {}, "cwd": {}, "env_file": {}, "tool_storage_dir": {},
	"session_files": {}, "sandbox": {}, "budget": {}, "project_instructions": {},
}

// layoutSessionFile is <sessions root>/<id>.json.
type layoutSessionFile struct {
	SchemaVersion int    `json:"schema_version"`
	Description   string `json:"description"`
	// Name is the display name; it defaults to the folder name.
	Name     string      `json:"name"`
	CWD      string      `json:"cwd"`
	Category string      `json:"category"`
	Scope    layoutScope `json:"scope"`
	// ModelDefaults is only read to refuse it: the model a profile uses is the
	// profile's own model_default, which the model-default route now writes.
	ModelDefaults json.RawMessage            `json:"model_defaults"`
	Settings      map[string]json.RawMessage `json:"settings"`
}

// defaultProfile is the profile a request that names none uses: the first profile in
// scope by ID. It is empty when the scope holds no profile.
func (s layoutSessionFile) defaultProfile(scoped *catalog.Catalog) string {
	if ids := scoped.IDs(); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// refuseModelDefaults fails a session.json that still carries the removed model_defaults
// key, so a model choice made before the change is never silently dropped.
func (s layoutSessionFile) refuseModelDefaults(path string) error {
	if s.ModelDefaults == nil {
		return nil
	}
	var entries bytes.Buffer
	if err := json.Compact(&entries, s.ModelDefaults); err != nil {
		entries.Reset()
		entries.Write(s.ModelDefaults)
	}
	return configError("%s: model_defaults was removed; the model a profile uses is its own model_default, which the model route now writes. Move each entry into that profile file's model_default (%s) and delete model_defaults", path, entries.String())
}

// layoutScope narrows what one session may use. Only profiles are scoped; models,
// toolsets and MCP servers are always all available, and a profile narrows models.
type layoutScope struct {
	Profiles catalog.Selection `json:"profiles"`
}

// layoutProfileFile is <profiles>/<category>/<name>.json.
type layoutProfileFile struct {
	SchemaVersion int    `json:"schema_version"`
	Description   string `json:"description"`
	// Name is the display name; it defaults to the profile ID.
	Name            string   `json:"name"`
	PromptDocuments []string `json:"prompt_documents"`
	PromptRole      string   `json:"prompt_role"`
	// Instructions is system-prompt text placed before the prompt documents. Kept raw so
	// that null and an empty string are refused and not read as absent.
	Instructions json.RawMessage            `json:"instructions"`
	Models       catalog.Selection          `json:"models"`
	ModelDefault string                     `json:"model_default"`
	Toolset      toolsetRefs                `json:"toolset"`
	Tools        json.RawMessage            `json:"tools"`
	Settings     map[string]json.RawMessage `json:"settings"`
}

// toolsetRefs is the toolset key of a profile: one "<category>/<name>" string or an
// array of them. Their order is the order of precedence when two toolsets clash.
type toolsetRefs []string

func (r *toolsetRefs) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == `""` {
		*r = nil
		return nil
	}
	ids, problem := parseToolsetRefs(data, false)
	if problem != "" {
		return errors.New("toolset " + problem)
	}
	*r = ids
	return nil
}

// parseToolsetRefs reads a string or an array of strings; null is none, problem follows "toolset ".
func parseToolsetRefs(data []byte, trim bool) (ids []string, problem string) {
	const shape = "must be a non-empty string or an array of distinct non-empty strings"
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil, ""
	}
	var single string
	if json.Unmarshal(data, &single) == nil {
		ids = []string{single}
	} else if json.Unmarshal(data, &ids) != nil || len(ids) == 0 {
		return nil, shape
	}
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if trim {
			id = strings.TrimSpace(id)
			ids[index] = id
		}
		if strings.TrimSpace(id) == "" {
			return nil, shape
		}
		if _, twice := seen[id]; twice {
			return nil, "lists " + id + " twice"
		}
		seen[id] = struct{}{}
	}
	return ids, ""
}

// layoutToolsetFile is <tools>/<category>/<name>.json. MCP stays raw because the order
// of its keys is the order of the servers.
type layoutToolsetFile struct {
	SchemaVersion int    `json:"schema_version"`
	Description   string `json:"description"`
	// Name is the display name shown beside the ID in the catalog editor; it changes no behavior.
	Name   string                     `json:"name"`
	Native map[string]json.RawMessage `json:"native"`
	// Extension is the toolset's tools.extension block, merged the same way as Native.
	Extension map[string]json.RawMessage `json:"extension"`
	MCP       json.RawMessage            `json:"mcp"`
	// Hooks has the shape of the settings key; kept raw so null is refused by the normalizer.
	Hooks json.RawMessage `json:"hooks"`
}

// layoutToolsetMCP is one server entry of a toolset.
type layoutToolsetMCP struct {
	Enabled   *bool           `json:"enabled"`
	DenyTools []string        `json:"deny_tools"`
	Resources *bool           `json:"resources"`
	Prompts   *bool           `json:"prompts"`
	Approval  json.RawMessage `json:"approval"`
}

// readLayoutFile decodes one catalog file strictly: an unknown key is an error that
// names the file, so a typo cannot silently drop a setting.
func readLayoutFile(path string, into any) error {
	contents, err := readRegularFile(path, maxInternalJSONFileBytes)
	if err != nil {
		return configError("%s: could not be read: %v", path, err)
	}
	return parseLayoutFile(path, contents, into)
}

// parseLayoutFile is readLayoutFile for contents already read; path only names the file in errors.
func parseLayoutFile(path string, contents []byte, into any) error {
	if strings.TrimSpace(string(contents)) == "" {
		return configError("%s: expected a JSON object", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return configError("%s: %v", path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return configError("%s: unexpected data after the JSON object", path)
	}
	return nil
}

// checkLayoutSettings refuses a settings object with a key outside layoutSettingKeys.
func checkLayoutSettings(settings map[string]json.RawMessage, where string) error {
	if err := rejectRemovedApproval(settings, "settings", where); err != nil {
		return err
	}
	if err := rejectRemovedOpenAIOAuthSetting(settings, where); err != nil {
		return err
	}
	var unknown []string
	for key := range settings {
		if _, ok := layoutSettingKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	candidates := make([]string, 0, len(layoutSettingKeys))
	for key := range layoutSettingKeys {
		candidates = append(candidates, key)
	}
	suggestion := ""
	if closest, distance := closestToolKey(unknown[0], candidates); closest != "" && distance <= 2 {
		suggestion = "; did you mean " + closest + "?"
	}
	return configError("%s: settings may not carry %s; it has its own file or field in the catalog layout%s", where, strings.Join(unknown, ", "), suggestion)
}
