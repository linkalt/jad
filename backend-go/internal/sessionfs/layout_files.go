package sessionfs

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
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
	"provider_retry": {}, "openai_oauth": {}, "max_run_tokens": {}, "approval": {}, "checkpoint": {},
	"hooks": {}, "image_resize": {}, "image_store": {}, "persistence": {}, "front_compaction": {},
	"compaction": {}, "http_debug": {},
}

// layoutSessionFile is <session>/session.json.
type layoutSessionFile struct {
	SchemaVersion int                        `json:"schema_version"`
	Description   string                     `json:"description"`
	CWD           string                     `json:"cwd"`
	Category      string                     `json:"category"`
	Scope         layoutScope                `json:"scope"`
	ActiveProfile string                     `json:"active_profile"`
	Settings      map[string]json.RawMessage `json:"settings"`
}

// layoutScope narrows what one session may use. Only profiles are scoped; models,
// toolsets and MCP servers are always all available, and a profile narrows models.
type layoutScope struct {
	Profiles catalog.Selection `json:"profiles"`
}

// layoutProfileFile is <profiles>/<category>/<name>.json.
type layoutProfileFile struct {
	SchemaVersion   int                        `json:"schema_version"`
	Description     string                     `json:"description"`
	PromptDocuments []string                   `json:"prompt_documents"`
	PromptRole      string                     `json:"prompt_role"`
	Models          catalog.Selection          `json:"models"`
	ModelDefault    string                     `json:"model_default"`
	Toolset         string                     `json:"toolset"`
	Tools           json.RawMessage            `json:"tools"`
	Settings        map[string]json.RawMessage `json:"settings"`
}

// layoutToolsetFile is <tools>/<category>/<name>.json. MCP stays raw because the order
// of its keys is the order of the servers.
type layoutToolsetFile struct {
	SchemaVersion int                        `json:"schema_version"`
	Description   string                     `json:"description"`
	Native        map[string]json.RawMessage `json:"native"`
	MCP           json.RawMessage            `json:"mcp"`
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
