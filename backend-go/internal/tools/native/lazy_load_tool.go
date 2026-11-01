package native

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// loadToolID is the tool's ID in tool_definition_paths.
const loadToolID = "native.load_tool"

// loadToolInputSchema is the schema of sample/tools/native/load_tool.json. The batch limit
// (tools.lazy_load_max) is enforced when the call runs, so it is not in the schema.
var loadToolInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "names":{
      "type":"array",
      "minItems":1,
      "uniqueItems":true,
      "description":"Tool names or tools.lazy-style patterns of the lazy tools to show and load, in one call: a tool name, mcp:<server>:<tool>, mcp:<server>:* or *. Omit names to list the lazy tools. Each pattern is limited to lazy tools; the total of matched tools is limited by the profile's tools.lazy_load_max.",
      "items":{"type":"string","minLength":1}
    }
  },
  "additionalProperties":false
}`)

// ChildLoadTool builds the load_tool a subagent child gets: the manifest definition, the set of
// the owning profile and that profile's hooks, the same as the parent's load_tool in
// BuildCatalogWithAsync. A child catalog holds it only when its agent lists load_tool.
func ChildLoadTool(config sessionfs.SessionConfig, lazy *tools.LazySet) (tools.Tool, error) {
	definitions, err := loadDefinitions(config.ToolDefinitionPaths)
	if err != nil {
		return tools.Tool{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
	}
	definition, err := requireNativeDefinition(config, definitions, "load_tool", loadToolID, loadToolInputSchema)
	if err != nil {
		return tools.Tool{}, err
	}
	tool, err := NewLoadToolTool(lazy, int(config.ProfileTools.LazyLoadMax))
	if err != nil {
		return tools.Tool{}, &sessionfs.ConfigError{Message: fmt.Sprintf("%s: %v", config.SettingPath, err)}
	}
	tool.Definition = definition
	return WithToolEventHooks(tool, config.Hooks, config.Model.APIKeyEnv), nil
}

// NewLoadToolTool builds load_tool for a session with lazy tools. With no names it lists the
// lazy tools as "name — summary" lines. With names it shows the full definition of each lazy
// tool a name selects (a tool name, mcp:<server>:<tool>, mcp:<server>:* or *) and records them as
// loaded, so the next call to one validates against the real schema. Loading enables nothing:
// the tool list the model sees does not change.
func NewLoadToolTool(set *tools.LazySet, loadMax int) (tools.Tool, error) {
	validator, err := tools.CompileSchema(loadToolInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native load_tool schema: %w", err)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          loadToolID,
			Source:      "native",
			Name:        sessionfs.LoadToolName,
			Description: "List the lazy tools (name and one-line summary), or show the full definitions of the named lazy tools and load them, in one call. Loading a tool does not enable it; a call to a lazy tool before it is loaded is refused.",
			InputSchema: append(json.RawMessage(nil), loadToolInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         true,
		ExampleOperation: json.RawMessage(`{"names":["example_tool"]}`),
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			names, _ := arguments["names"].([]any)
			if len(names) == 0 {
				return listLazyTools(set.Entries()), nil
			}
			patterns := make([]string, 0, len(names))
			for _, value := range names {
				name, _ := value.(string)
				patterns = append(patterns, name)
			}
			matches := set.Match(patterns)
			count := matchCount(matches)
			if count > loadMax {
				return tools.ExecutionError(execution.CallID, tools.StatusError, "batch_too_large",
					fmt.Sprintf("matched %d lazy tools, the limit is %d (tools.lazy_load_max)", count, loadMax),
					tools.Information{Key: "limit", Value: loadMax},
					tools.Information{Key: "matched", Value: count}), nil
			}
			return loadLazyTools(set, patterns, matches), nil
		},
	}, nil
}

// listLazyTools lists every lazy tool, sorted by name, as "name — summary".
func listLazyTools(entries []tools.LazyEntry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Tool.Definition.Name+" — "+entry.Summary)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "No lazy tools."
	}
	return strings.Join(lines, "\n")
}

// matchCount is the number of distinct lazy tools the patterns select, a tool counted once
// however many patterns select it.
func matchCount(matches [][]tools.LazyEntry) int {
	seen := map[string]bool{}
	for _, entries := range matches {
		for _, entry := range entries {
			seen[entry.Tool.Definition.ID] = true
		}
	}
	return len(seen)
}

// loadLazyTools shows the lazy tools each pattern selects, one section per pattern in the order
// sent, and loads them in one write. A tool is shown in full under the first section that selects
// it; a later section that selects it again says so. A pattern that selects nothing, or a name that
// is not a lazy tool, gets its own line, and the others still load. A newly loaded tool is listed
// from the next request, since a request's tools array is built before its calls run. The caller
// has checked the count against the limit and the names for duplicates.
func loadLazyTools(set *tools.LazySet, patterns []string, matches [][]tools.LazyEntry) string {
	added, err := set.LoadMatches(matches)
	if err != nil {
		return fmt.Sprintf("Could not record the loaded lazy tools: %v", err)
	}
	shown := map[string]bool{}
	sections := make([]string, 0, len(patterns))
	for index, pattern := range patterns {
		entries := matches[index]
		if len(entries) == 0 {
			sections = append(sections, noLazyToolLine(pattern))
			continue
		}
		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			definition := entry.Tool.Definition
			if shown[definition.ID] {
				lines = append(lines, definition.Name+": already listed above")
				continue
			}
			shown[definition.ID] = true
			lines = append(lines, fmt.Sprintf("%s\n\n%s\n\ninputSchema: %s", definition.Name, definition.Description, definition.InputSchema))
		}
		sections = append(sections, strings.Join(lines, "\n\n"))
	}
	text := strings.Join(sections, "\n\n---\n\n")
	if added > 0 {
		return "Loaded. Available from the next request.\n\n" + text
	}
	return text
}

// noLazyToolLine is the line for a pattern or name that selects no lazy tool of the profile.
func noLazyToolLine(pattern string) string {
	if tools.IsLazyPattern(pattern) {
		return fmt.Sprintf("no lazy tool matches %s", pattern)
	}
	return fmt.Sprintf("%s: not a lazy tool of this profile. Call load_tool with no names to list them.", pattern)
}
