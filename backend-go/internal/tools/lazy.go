package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
)

// lazyToolsFile is the file, in the session's state folder, that records which lazy tools
// load_tool has loaded.
const lazyToolsFile = "lazy_tools.json"

// lazyToolsState is the content of lazy_tools.json.
type lazyToolsState struct {
	Fingerprint string   `json:"fingerprint"`
	Loaded      []string `json:"loaded"`
}

// LazyEntry is one lazy tool of a profile: the tool the session registered and the one-line
// summary the model sees until load_tool shows the full definition.
type LazyEntry struct {
	Tool    Tool
	Summary string
}

// LazySet is the lazy tools of one profile and the ones load_tool has loaded for the session.
// It is opened for every prepared run from the state folder, so the file is its only state.
// OpenLazySet takes the config; Bind attaches the registered tools that the config's entries
// match, once they exist.
type LazySet struct {
	path         string
	fingerprint  string
	summaryBytes int
	entries      []string
	bound        []LazyEntry
	bindings     map[string]bool
	gated        map[string]bool
	// mu guards loaded: load_tool writes it while a lazy call and the per-request tool list read it.
	mu     sync.Mutex
	loaded map[string]bool
	shown  func(Definition) bool
}

// LazyFingerprint identifies a lazy configuration: the sorted, de-duplicated tools.lazy entries
// and the summary bound, since the bound changes the summary the model sees. The profile name, the
// session ID and the registered tool list are not part of it, so a tool that a wildcard entry
// picks up later is refused until loaded, and a config change clears the loaded set.
func LazyFingerprint(entries []string, summaryBytes int) string {
	unique := map[string]bool{}
	for _, entry := range entries {
		unique[entry] = true
	}
	sorted := make([]string, 0, len(unique))
	for entry := range unique {
		sorted = append(sorted, entry)
	}
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n") + "\x00" + strconv.Itoa(summaryBytes)))
	return hex.EncodeToString(sum[:])
}

// OpenLazySet reads the loaded set of the session in stateDir for the given tools.lazy entries.
// A missing or unreadable file, or one whose fingerprint differs from the current config, gives
// an empty set and rewrites the file. Missing or unreadable is logged. An empty stateDir means
// the root has no catalog layout, which lazy tools cannot use. shown reports whether the profile
// shows a tool; a tool it hides is never bound, so no name selects it. Nil shows every tool.
func OpenLazySet(stateDir string, entries []string, summaryBytes int, shown func(Definition) bool, logger *slog.Logger) (*LazySet, error) {
	if stateDir == "" {
		return nil, errors.New("tools.lazy needs a catalog layout: the session has no state folder")
	}
	set := &LazySet{
		path:         filepath.Join(stateDir, lazyToolsFile),
		fingerprint:  LazyFingerprint(entries, summaryBytes),
		summaryBytes: summaryBytes,
		entries:      append([]string(nil), entries...),
		bindings:     map[string]bool{},
		gated:        map[string]bool{},
		loaded:       map[string]bool{},
		shown:        shown,
	}
	state, err := readLazyToolsState(set.path)
	if err != nil && logger != nil {
		logger.Warn("lazy tool state unreadable; no lazy tool is loaded", "path", set.path, "error", err)
	}
	if err == nil && state.Fingerprint == set.fingerprint {
		for _, id := range state.Loaded {
			set.loaded[id] = true
		}
		return set, nil
	}
	if err := set.save(); err != nil {
		return nil, fmt.Errorf("write %s: %w", set.path, err)
	}
	return set, nil
}

func readLazyToolsState(path string) (lazyToolsState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return lazyToolsState{}, nil
		}
		return lazyToolsState{}, err
	}
	var state lazyToolsState
	if err := json.Unmarshal(data, &state); err != nil {
		return lazyToolsState{}, err
	}
	return state, nil
}

// Bind attaches the registered tools that the config's entries match. Each matching tool
// becomes lazy, with the summary of its description. A tool already bound is skipped, so
// binding a list twice never lists a tool twice. An entry that matches nothing is not an error
// here: the profile check reports it before the catalog is built. The native and the MCP
// tools are bound in turn, and their sets are disjoint because an entry matches only its own kind.
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
	for _, entry := range s.bound {
		if entry.Tool.Definition.ID == tool.Definition.ID {
			return WithLazyGate(entry, s), true
		}
	}
	return tool, false
}

// shows reports whether the profile shows the tool; with no predicate every tool is shown.
func (s *LazySet) shows(definition Definition) bool {
	return s.shown == nil || s.shown(definition)
}

func (s *LazySet) isLazy(definition Definition) bool {
	for _, entry := range s.entries {
		if EntryMatches(entry, definition) {
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

// Loaded reports whether load_tool has loaded the tool with this ID.
func (s *LazySet) Loaded(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded[id]
}

// Entries returns the bound lazy tools, in the order they were bound.
func (s *LazySet) Entries() []LazyEntry {
	if s == nil {
		return nil
	}
	return append([]LazyEntry(nil), s.bound...)
}

// Load marks the named lazy tool loaded. The name must match a bound lazy tool; a name that
// matches none is refused and nothing is written.
func (s *LazySet) Load(name string) error {
	matched, err := s.LoadAll([]string{name})
	if err != nil {
		return err
	}
	if !matched[0] {
		return fmt.Errorf("%s is not a lazy tool of this profile", name)
	}
	return nil
}

// LoadAll marks loaded every bound lazy tool that a name selects (see Match). It reports, in the
// order given, which names matched; a name that matches none is left out and does not stop the
// others. The set is written once, after the lookup, and only when a matched name was not
// loaded before, through a temporary file and a rename. A failed write undoes the new marks.
func (s *LazySet) LoadAll(names []string) ([]bool, error) {
	matches := s.Match(names)
	if _, err := s.LoadMatches(matches); err != nil {
		return nil, err
	}
	matched := make([]bool, len(names))
	for index, entries := range matches {
		matched[index] = len(entries) > 0
	}
	return matched, nil
}

// LoadMatches marks loaded every tool the matches select, which Match returned, and writes the set
// once if any was not loaded before. A tool selected by several patterns is loaded once. It returns
// the number of tools newly loaded.
func (s *LazySet) LoadMatches(matches [][]LazyEntry) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var added []string
	for _, entries := range matches {
		for _, entry := range entries {
			id := entry.Tool.Definition.ID
			if !s.loaded[id] && !slices.Contains(added, id) {
				added = append(added, id)
			}
		}
	}
	if len(added) == 0 {
		return 0, nil
	}
	for _, id := range added {
		s.loaded[id] = true
	}
	if err := s.save(); err != nil {
		for _, id := range added {
			delete(s.loaded, id)
		}
		return 0, err
	}
	return len(added), nil
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
// tools.lazy takes: "*" for every tool, mcp:<server>:<tool> and mcp:<server>:* as EntryMatches
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

// save writes the set through a temporary file and a rename, so a reader never sees half a file.
// Loaded IDs that no longer match a tool are kept: the file says what was loaded, and the
// bound list decides what is callable.
func (s *LazySet) save() error {
	loaded := make([]string, 0, len(s.loaded))
	for id := range s.loaded {
		loaded = append(loaded, id)
	}
	sort.Strings(loaded)
	data, err := json.Marshal(lazyToolsState{Fingerprint: s.fingerprint, Loaded: loaded})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), lazyToolsFile+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}

// WithLazyGate wraps a lazy tool for the session's catalog. Its schema, executor and report are the
// real tool's. A call before load is refused with tool_not_loaded, whatever its arguments: the
// loaded check runs before the real schema, so the real schema is never run against an unloaded
// tool's call. A call after load runs through the real tool's own catalog, so validation and output
// match an eager tool. The gate is applied to the raw tool, before the event hooks, so a refused call
// still fires pre_tool and post_tool hooks like any tool. The tool is listed only while it is loaded
// (see Listed).
func WithLazyGate(entry LazyEntry, set *LazySet) Tool {
	real := entry.Tool
	gated := real
	gated.Listed = func() bool { return set.Loaded(real.Definition.ID) }
	gated.Refused = func(ExecutionContext) bool { return !set.Loaded(real.Definition.ID) }
	// The real tool runs through a catalog of its own, so its validation against the real
	// schema, its bound on the report and its output match what an eager tool would give.
	inner, err := NewCatalog([]Tool{real})
	if err != nil {
		panic(fmt.Sprintf("lazy tool %s: %v", real.Definition.Name, err))
	}
	refused := func(execution ExecutionContext) string {
		return ExecutionError(execution.CallID, StatusError, "tool_not_loaded",
			fmt.Sprintf("Tool %s is lazy and not loaded yet", real.Definition.Name),
			Information{Key: "tool", Value: real.Definition.Name},
			Information{Key: "load", Value: sessionfs.LoadToolName})
	}
	if real.ExecuteOutput != nil {
		gated.ExecuteOutput = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (json.RawMessage, error) {
			if !set.Loaded(real.Definition.ID) {
				return json.Marshal(refused(execution))
			}
			return callInner(ctx, inner, arguments, execution)
		}
		return gated
	}
	gated.Execute = func(ctx context.Context, arguments map[string]any, execution ExecutionContext) (string, error) {
		if !set.Loaded(real.Definition.ID) {
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
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	call, err := json.Marshal(map[string]string{"type": "function_call", "call_id": execution.CallID, "name": execution.ToolName, "arguments": string(encoded)})
	if err != nil {
		return nil, err
	}
	envelope, err := inner.ExecuteFunctionCall(ctx, call, execution)
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
