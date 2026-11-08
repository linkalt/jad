package native

import (
	"bytes"
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

// loadToolInputSchema is the schema of sample/tools/native/load_tool.json. It is flat: no oneOf
// or anyOf, which the Anthropic adapter strips from the top level. Which of the three
// uses a call is (list, describe, run) follows from the keys it sends. The batch limit
// (tools.lazy_load_max) is enforced when the call runs, so it is not in the schema.
var loadToolInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "names":{
      "type":"array",
      "minItems":1,
      "uniqueItems":true,
      "description":"Describe lazy tools: their tool names or tools.lazy-style patterns, in one call: a tool name, mcp:<server>:<tool>, mcp:<server>:* or *. Returns each tool's name, id, description and inputSchema. Omit names and run to list the lazy tools. The total of matched tools is limited by the profile's tools.lazy_load_max. Not together with run.",
      "items":{"type":"string","minLength":1}
    },
    "run":{
      "type":"string",
      "minLength":1,
      "description":"Run one lazy tool: its exact tool name or mcp:<server>:<tool>, no pattern. Give its arguments in arguments, as its inputSchema says (see names). Not together with names."
    },
    "arguments":{
      "type":"object",
      "description":"The arguments of the tool named in run. Defaults to {}. Used only with run.",
      "default":{}
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

// NewLoadToolTool builds load_tool, the runner for a session's lazy tools. A lazy tool is not in
// the model's tools array, so the array never changes with its use. With no names and no run it
// lists the lazy tools as "name — summary" lines. With names it describes the lazy tools they
// select (a tool name, mcp:<server>:<tool>, mcp:<server>:* or *): name, id, description and
// inputSchema. With run it runs one lazy tool, named exactly, through the catalog that runs
// load_tool (ExecutionContext.Dispatcher), with the tool's arguments in arguments, and returns the
// tool's own output. Only the lazy tools the dispatching catalog holds are listed, described or run,
// so a plan-mode or narrowed catalog does not show what it cannot call. Nothing is written to the lazy set.
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
			Description: loadToolDescription,
			InputSchema: append(json.RawMessage(nil), loadToolInputSchema...),
		},
		Validator:        validator,
		ReadOnly:         true,
		ExampleOperation: json.RawMessage(`{"run":"example_tool","arguments":{}}`),
		Delegate:         loadToolDelegate,
		ExecuteOutput: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
			_, hasRun := arguments["run"]
			names, hasNames := arguments["names"].([]any)
			_, hasArguments := arguments["arguments"]
			switch {
			case hasRun && hasNames:
				return textOutput(invalidLoadToolInput(execution, "names and run cannot be combined: describe with names, then run with run"))
			case hasRun:
				return runLazyTool(ctx, set, arguments, execution)
			case hasArguments:
				return textOutput(invalidLoadToolInput(execution, "arguments is only used with run"))
			case !hasNames:
				return textOutput(listLazyTools(visibleEntries(set.Entries(), execution.Dispatcher)))
			}
			patterns := make([]string, 0, len(names))
			for _, value := range names {
				name, _ := value.(string)
				patterns = append(patterns, name)
			}
			matches := visibleMatches(set.Match(patterns), execution.Dispatcher)
			count := matchCount(matches)
			if count > loadMax {
				return textOutput(tools.ExecutionError(execution.CallID, tools.StatusError, "batch_too_large",
					fmt.Sprintf("matched %d lazy tools, the limit is %d (tools.lazy_load_max)", count, loadMax),
					tools.Information{Key: "limit", Value: loadMax},
					tools.Information{Key: "matched", Value: count}))
			}
			return textOutput(describeLazyTools(patterns, matches))
		},
	}, nil
}

const loadToolDescription = "Lazy tools are not in your tool list. With no arguments, list them (name and one-line summary). With names, describe lazy tools: each one's name, id, description and inputSchema. With run and arguments, run one lazy tool by its exact name or mcp:<server>:<tool>; arguments are its arguments as its inputSchema says. A lazy tool is never called by its own name: a direct call is refused. names and run cannot be combined."

// loadToolDelegate is Tool.Delegate of load_tool: a call with run (and no names) runs the tool
// run names with the arguments in arguments, none being {}. Anything else runs nothing.
func loadToolDelegate(arguments map[string]any) (string, map[string]any, bool) {
	target, _ := arguments["run"].(string)
	if target == "" {
		return "", nil, false
	}
	if _, hasNames := arguments["names"]; hasNames {
		return "", nil, false
	}
	targetArguments := map[string]any{}
	if value, present := arguments["arguments"]; present {
		object, isObject := value.(map[string]any)
		if !isObject {
			return "", nil, false
		}
		targetArguments = object
	}
	return target, targetArguments, true
}

// textOutput is a text result as the output value of a call.
func textOutput(text string) (json.RawMessage, error) {
	return json.Marshal(text)
}

func invalidLoadToolInput(execution tools.ExecutionContext, summary string) string {
	return tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "invalid_input", summary)
}

// visibleEntries keeps the lazy tools the dispatching catalog holds. With no dispatcher (a call
// made outside a catalog) every tool is kept.
func visibleEntries(entries []tools.LazyEntry, dispatcher tools.Dispatcher) []tools.LazyEntry {
	if dispatcher == nil {
		return entries
	}
	kept := make([]tools.LazyEntry, 0, len(entries))
	for _, entry := range entries {
		if dispatcher.Has(entry.Tool.Definition.Name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

func visibleMatches(matches [][]tools.LazyEntry, dispatcher tools.Dispatcher) [][]tools.LazyEntry {
	kept := make([][]tools.LazyEntry, len(matches))
	for index, entries := range matches {
		kept[index] = visibleEntries(entries, dispatcher)
	}
	return kept
}

// runLazyTool runs the one lazy tool the run argument names through the dispatching catalog and
// returns its output value. The call keeps the call ID, and the execution context passes through
// as it is, with ViaRunner set: the tool answers as it does for an eager tool, with its own
// schema check, its own hooks and its own output. A result it gives for invalid arguments is
// the tool's own.
func runLazyTool(ctx context.Context, set *tools.LazySet, arguments map[string]any, execution tools.ExecutionContext) (json.RawMessage, error) {
	target, targetArguments, ok := loadToolDelegate(arguments)
	if !ok {
		return textOutput(invalidLoadToolInput(execution, "arguments must be an object"))
	}
	if tools.IsLazyPattern(target) {
		return textOutput(invalidLoadToolInput(execution, fmt.Sprintf("run takes one exact tool name or mcp:<server>:<tool>, not the pattern %s; list the lazy tools with %s and no arguments", target, sessionfs.LoadToolName)))
	}
	entries := set.Match([]string{target})[0]
	if len(entries) != 1 {
		return textOutput(tools.ExecutionError(execution.CallID, tools.StatusInvalidInput, "unknown_lazy_tool",
			fmt.Sprintf("%s is not a lazy tool of this profile; list the lazy tools with %s and no arguments", target, sessionfs.LoadToolName),
			tools.Information{Key: "tool", Value: target}))
	}
	name := entries[0].Tool.Definition.Name
	if execution.Dispatcher == nil || !execution.Dispatcher.Has(name) {
		return textOutput(tools.UnknownToolError(execution.CallID, name))
	}
	execution.ViaRunner = true
	return tools.DispatchCall(ctx, execution.Dispatcher, name, targetArguments, execution)
}

// listLazyTools lists every lazy tool, sorted by name, as "name — summary", and says how to describe
// and run them.
func listLazyTools(entries []tools.LazyEntry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Tool.Definition.Name+" — "+entry.Summary)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "No lazy tools."
	}
	return strings.Join(lines, "\n") + "\n\n" + loadToolUsage
}

// loadToolUsage ends a list and a description.
const loadToolUsage = "Describe tools with names for their full definition; run one with run (its name) and arguments."

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

// describeLazyTools describes the lazy tools each pattern selects, one section per pattern in the order
// sent: name, id, description and inputSchema of each tool. A tool is described under the first section
// that selects it; a later section that selects it again says so. A pattern that selects nothing, or a
// name that is not a lazy tool, gets its own line, and the others are still described. The caller has
// checked the count against the limit.
func describeLazyTools(patterns []string, matches [][]tools.LazyEntry) string {
	shown := map[string]bool{}
	sections := make([]string, 0, len(patterns))
	described := false
	for index, pattern := range patterns {
		entries := matches[index]
		if len(entries) == 0 {
			sections = append(sections, noLazyToolLine(pattern))
			continue
		}
		blocks := make([]string, 0, len(entries))
		for _, entry := range entries {
			definition := entry.Tool.Definition
			if shown[definition.ID] {
				blocks = append(blocks, definition.Name+": already described above")
				continue
			}
			shown[definition.ID] = true
			described = true
			schema := string(definition.InputSchema)
			var compact bytes.Buffer
			if json.Compact(&compact, definition.InputSchema) == nil {
				schema = compact.String()
			}
			blocks = append(blocks, "name: "+definition.Name+"\nid: "+definition.ID+"\ndescription: "+definition.Description+"\ninputSchema: "+schema)
		}
		sections = append(sections, strings.Join(blocks, "\n\n"))
	}
	text := strings.Join(sections, "\n\n")
	if described {
		text += "\n\n" + loadToolUsage
	}
	return text
}

// noLazyToolLine is the line for a pattern or name that selects no lazy tool of the profile.
func noLazyToolLine(pattern string) string {
	if tools.IsLazyPattern(pattern) {
		return fmt.Sprintf("no lazy tool matches %s", pattern)
	}
	return fmt.Sprintf("%s: not a lazy tool of this profile. Call load_tool with no arguments to list them.", pattern)
}
