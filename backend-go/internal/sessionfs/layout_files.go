package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"jad/backend-go/internal/catalog"
)

// Layout is where the catalog layout keeps its files (sample/jad shows one). Every
// directory is absolute. Defaults holds the root setting `defaults`; ToolDefinitionPaths
// the root `libraries.tool_definition_paths`.
type Layout struct {
	Profiles            string
	Models              string
	Tools               string
	MCP                 string
	State               string
	Defaults            map[string]json.RawMessage
	ToolDefinitionPaths []string
}

// layoutSettingKeys are the setting keys a `settings` object (session, profile) and the
// root `defaults` may carry. Everything else has its own file or field in the new layout.
var layoutSettingKeys = map[string]struct{}{
	"statecontext": {}, "max_tool_rounds": {}, "max_parallel_tool_calls": {}, "tool_call_scheduling": {},
	"provider_retry": {}, "provider_stream": {}, "openai_oauth": {}, "max_run_tokens": {}, "approval": {}, "checkpoint": {},
	"hooks": {}, "image_resize": {}, "image_store": {}, "persistence": {}, "front_compaction": {},
	"compaction": {}, "http_debug": {}, "cwd": {}, "env_file": {}, "tool_storage_dir": {},
	"session_files": {},
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
	MCP    json.RawMessage            `json:"mcp"`
	// Hooks has the shape of the settings key; kept raw so null is refused by the normalizer.
	Hooks json.RawMessage `json:"hooks"`
}

// layoutToolsetMCP is one server entry of a toolset.
type layoutToolsetMCP struct {
	Enabled   *bool    `json:"enabled"`
	DenyTools []string `json:"deny_tools"`
	Approval  *string  `json:"approval"`
}

// readLayoutFile decodes one catalog file strictly: an unknown key is an error that
// names the file, so a typo cannot silently drop a setting.
func readLayoutFile(path string, into any) error {
	contents, err := os.ReadFile(path)
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
	if _, err := decoder.Token(); err == nil {
		return configError("%s: unexpected data after the JSON object", path)
	}
	return nil
}

// checkLayoutSettings refuses a settings object with a key outside layoutSettingKeys.
func checkLayoutSettings(settings map[string]json.RawMessage, where string) error {
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
	return configError("%s: settings may not carry %s; it has its own file or field in the catalog layout", where, strings.Join(unknown, ", "))
}

// LayoutSettingKey reports whether name may appear in a session, profile or root
// `settings`/`defaults` object of the catalog layout.
func LayoutSettingKey(name string) bool {
	_, ok := layoutSettingKeys[name]
	return ok
}
