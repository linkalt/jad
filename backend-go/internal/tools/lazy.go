package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
)

// LazyEntry is one lazy tool of a profile: the tool the session registered and the one-line
// summary the model sees; load_tool describes it (full definition) and runs it on request.
type LazyEntry struct {
	Tool    Tool
	Summary string
}

// LazySet is the lazy tools of one profile: the MCP and extension tools that its lazy entries
// (tools.native.load_tool.lazy) name (a native tool is never lazy). It keeps no state outside the process: the entries and the
// registered tools decide it, so a session has no file for it. NewLazySet takes the config; Bind
// attaches the registered tools that the config's entries match, once they exist.
type LazySet struct {
	summaryBytes int
	entries      []string
	bound        []LazyEntry
	bindings     map[string]bool
	gated        map[string]bool
	shown        func(Definition) bool
}

// NewLazySet builds the lazy set for the given lazy entries (tools.native.load_tool.lazy). shown reports whether the profile
// shows a tool; a tool it hides is never bound, so no name selects it. Nil shows every tool.
func NewLazySet(entries []string, summaryBytes int, shown func(Definition) bool) *LazySet {
	return &LazySet{
		summaryBytes: summaryBytes,
		entries:      append([]string(nil), entries...),
		bindings:     map[string]bool{},
		gated:        map[string]bool{},
		shown:        shown,
	}
}

// Bind attaches the registered tools that the config's entries match. Each matching tool
// becomes lazy, with the summary of its description. Only an MCP or an extension tool is bound
// (CanBeLazy): a native tool is skipped whatever an entry says. A tool already bound is skipped, so
// binding a list twice never lists a tool twice. An entry that matches nothing is not an error
// here: the profile check reports it before the catalog is built. The extension and the MCP tools
// are bound in turn, and their sets are disjoint because an entry matches only its own kind.
func (s *LazySet) Bind(registered []Tool) {
	if s == nil {
		return
	}
	for _, tool := range registered {
		if s.bindings[tool.Definition.ID] || !s.isLazy(tool.Definition) || !s.shows(tool.Definition) {
			continue
		}
		s.bindings[tool.Definition.ID] = true
		s.bound = append(s.bound, LazyEntry{Tool: tool, Summary: LazySummary(tool.Definition.Description, s.summaryBytes)})
	}
}

// Gate returns the lazy gate of a tool the set has bound, and false for any other tool, and for
// a tool already gated, so a tool is never wrapped twice.
func (s *LazySet) Gate(tool Tool) (Tool, bool) {
	if s == nil || !s.bindings[tool.Definition.ID] || s.gated[tool.Definition.ID] {
		return tool, false
	}
	s.gated[tool.Definition.ID] = true
	return s.GateFor(tool)
}

// GateFor returns a fresh lazy gate of the tool it is given, and false for any other tool: the set
// must have bound the tool's ID and the tool must be able to be lazy. The gate wraps the given tool,
// never the set's own bound copy, so a catalog whose copy differs from the owner's (a toolset's
// connection and hooks) keeps its copy; the set supplies only the decision. It does not mark the
// tool as gated, so any number of catalogs can gate their own copies of one lazy tool, all reading
// this set. Gate keeps its one-shot contract for the owning catalog.
func (s *LazySet) GateFor(tool Tool) (Tool, bool) {
	if s == nil || !CanBeLazy(tool.Definition) || !s.bindings[tool.Definition.ID] {
		return tool, false
	}
	return WithLazyGate(LazyEntry{Tool: tool}, s), true
}

// shows reports whether the profile shows the tool; with no predicate every tool is shown.
func (s *LazySet) shows(definition Definition) bool {
	return s.shown == nil || s.shown(definition)
}

// isLazy reports whether an entry names the tool and the tool can be lazy: a native tool never is.
func (s *LazySet) isLazy(definition Definition) bool {
	for _, entry := range s.entries {
		if LazyEntryMatches(entry, definition) {
			return true
		}
	}
	return false
}

// LazySummary is the first line of a tool description, cut to maxBytes on a rune boundary.
func LazySummary(description string, maxBytes int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(description), "\n")
	return utf8Prefix(strings.TrimSpace(line), maxBytes)
}

// utf8Prefix returns the longest prefix of text that is at most maxBytes bytes and ends on a
// rune boundary, so a summary never ends in half a character.
func utf8Prefix(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// Entries returns the bound lazy tools, in the order they were bound.
func (s *LazySet) Entries() []LazyEntry {
	if s == nil {
		return nil
	}
	return append([]LazyEntry(nil), s.bound...)
}

// Match returns, for each name in order, the bound lazy tools the name selects, sorted by ID.
// Only bound tools are selected, so a tool the profile hides or does not list as lazy never matches.
func (s *LazySet) Match(names []string) [][]LazyEntry {
	matches := make([][]LazyEntry, len(names))
	if s == nil {
		return matches
	}
	for index, name := range names {
		for _, entry := range s.bound {
			if nameSelects(name, entry.Tool.Definition) {
				matches[index] = append(matches[index], entry)
			}
		}
		sort.Slice(matches[index], func(a, b int) bool {
			return matches[index][a].Tool.Definition.ID < matches[index][b].Tool.Definition.ID
		})
	}
	return matches
}

// nameSelects reports whether one load_tool name selects a bound tool. The names are the ones
// the lazy list takes: "*" for every tool, mcp:<server>:<tool> and mcp:<server>:* as EntryMatches
// reads them, and otherwise a tool's provider name.
func nameSelects(name string, definition Definition) bool {
	if name == "*" {
		return true
	}
	if strings.HasPrefix(name, mcpEntryPrefix) {
		return EntryMatches(name, definition)
	}
	return definition.Name == name
}

// IsLazyPattern reports whether a load_tool name is a pattern, "*" or mcp:<server>:*, rather than
// one tool's name.
func IsLazyPattern(name string) bool {
	return name == "*" || (strings.HasPrefix(name, mcpEntryPrefix) && strings.HasSuffix(name, ":*"))
}

// WithLazyGate wraps a lazy tool for the session's catalog. Its schema, executor and report are the
// real tool's. The tool is never listed: the model's tools array does not hold it, so the array
// is the same whether or not the model uses the tool, and a provider's prompt cache is not
// invalidated by a lazy tool. The model describes and runs it through load_tool, which dispatches
// the call through the executing catalog with ExecutionContext.ViaRunner set. A call that is not
// via the runner (the model naming the tool directly) is refused with lazy_tool_not_loaded, whatever its
// arguments: the check runs before the real schema, so the real schema is never run against a direct
// call. A call via the runner runs through the real tool's own catalog, so validation and output
// match an eager tool. The gate is applied to the raw tool, before the event hooks, so a refused call
// still fires pre_tool and post_tool hooks like any tool.
func WithLazyGate(entry LazyEntry, _ *LazySet) Tool {
	real := entry.Tool
	gated := real
	gated.Listed = func() bool { return false }
	gated.Lazy = true
	gated.Refused = func(execution ExecutionContext) bool { return !execution.ViaRunner }
	// The real tool runs through a catalog of its own, so its validation against the real
	// schema, its bound on the report and its output match what an eager tool would give.
	inner, err := NewCatalog([]Tool{real})
	if err != nil {
		panic(fmt.Sprintf("lazy tool %s: %v", real.Definition.Name, err))
	}
	refused := func(execution ExecutionContext) string {
		return ExecutionError(execution.CallID, StatusError, "lazy_tool_not_loaded",
			fmt.Sprintf("Tool %s is lazy: it is not called directly. Call %s with run %q and the tool's arguments in arguments; describe it first with %s names [%q]", real.Definition.Name, sessionfs.LoadToolName, real.Definition.Name, sessionfs.LoadToolName, real.Definition.Name),
			Information{Key: "tool", Value: real.Definition.Name},
			Information{Key: "run", Value: sessionfs.LoadToolName})
	}
	if real.ExecuteOutput != nil {
		gated.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
			if !execution.ViaRunner {
				return json.Marshal(refused(execution))
			}
			return callInner(ctx, inner, arguments, execution)
		}
		return gated
	}
	gated.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
		if !execution.ViaRunner {
			return refused(execution), nil
		}
		output, err := callInner(ctx, inner, arguments, execution)
		if err != nil {
			return "", err
		}
		var text string
		if err := json.Unmarshal(output, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	return gated
}

// callInner runs one call through the catalog of the real tool and returns its output value.
// The catalog fills the call's name and call ID itself, so the envelope carries only the
// arguments as the provider sends them: a JSON string.
func callInner(ctx context.Context, inner *Catalog, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
	return DispatchCall(ctx, inner, execution.ToolName, arguments, execution)
}

// FunctionCall is the function_call item of a call to the tool named name with the given arguments,
// as the provider sends it: the arguments are a JSON string.
func FunctionCall(callID, name string, arguments map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"type": "function_call", "call_id": callID, "name": name, "arguments": string(encoded)})
}

// DispatchCall runs one call of the tool named name through the dispatcher, with the given
// arguments and the execution context as it is, and returns the output value of the
// function_call_output the dispatcher answers with. The call keeps the call ID of the execution, so a
// runner's nested call is the same call to the model.
func DispatchCall(ctx context.Context, dispatcher Dispatcher, name string, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
	call, err := FunctionCall(execution.CallID, name, arguments)
	if err != nil {
		return nil, err
	}
	envelope, err := dispatcher.ExecuteFunctionCall(ctx, call, execution)
	if err != nil {
		return nil, err
	}
	var result struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(envelope, &result); err != nil {
		return nil, err
	}
	return result.Output, nil
}
