package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const codeContextToolID = "native.code_context"

var codeContextInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["read","add","update","drop","get","list","find","clear"],"description":"read prints a range and saves it; add saves a range with its description; update changes one (id kept); drop removes; get prints the saved code; list shows the entries as YAML; find searches the saved descriptions and paths; clear deletes the task."},
    "task":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$","description":"Task key the entries belong to, for example T-023."},
    "id":{"type":"integer","minimum":1,"description":"Entry id for update, or for add with from_task."},
    "ids":{"type":"array","items":{"type":"integer","minimum":1},"minItems":1,"description":"Entry ids for drop or get; get without ids prints every entry."},
    "path":{"type":"string","minLength":1,"description":"Source file, relative to cwd."},
    "start":{"type":"integer","minimum":1,"description":"First line, 1-based."},
    "end":{"type":"integer","minimum":1,"description":"Last line, inclusive."},
    "description":{"type":"string","minLength":1,"description":"What the code does, its inputs, outputs and constraints, and why it matters for the task; no line numbers. Optional on read: add it later with update."},
    "priority":{"type":"integer","minimum":1,"maximum":6,"description":"1 target, 2 callers and callees, 3 interfaces and implementations, 4 data and persistence, 5 tests, 6 secondary. get leaves out the highest numbers first."},
    "since":{"type":"string","pattern":"^[0-9a-f]{12}$","description":"get: the since token an earlier get printed. Entries whose code and description are unchanged since then are named, not printed again."},
    "mode":{"type":"string","enum":["code","outline"],"description":"get: code (default) prints descriptions and code; outline prints descriptions and ranges only."},
    "max_priority":{"type":"integer","minimum":1,"maximum":6,"description":"get, list and find: only entries of this priority or a lower number."},
    "query":{"type":"string","minLength":1,"description":"find: words to look for in the saved descriptions and paths."},
    "all_tasks":{"type":"boolean","description":"find: also search the other tasks of the storage; their entries are shown only while their code is unchanged."},
    "from_task":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$","description":"add: copy entry id of this other task (range, description, priority) once its code is checked unchanged."}
  },
  "required":["action","task"],
  "additionalProperties":false,
  "allOf":[
    {"if":{"properties":{"action":{"const":"read"}},"required":["action"]},"then":{"required":["path","start","end"]}},
    {"if":{"properties":{"action":{"const":"add"}},"required":["action"]},"then":{"anyOf":[{"required":["from_task","id"]},{"required":["path","start","end","description","priority"]}]}},
    {"if":{"properties":{"action":{"const":"update"}},"required":["action"]},"then":{"required":["id"]}},
    {"if":{"properties":{"action":{"const":"drop"}},"required":["action"]},"then":{"required":["ids"]}},
    {"if":{"properties":{"action":{"const":"find"}},"required":["action"]},"then":{"required":["query"]}}
  ]
}`)

// NewCodeContextTool constructs native.code_context: per task, a list of exact source
// ranges with descriptions. get reads each range again and prints its code. A range whose
// saved bytes now occur exactly once elsewhere in the file is moved there; one whose bytes
// changed is listed for the caller to re-read and update, never printed as code. The tool
// never computes a new range from edits.
func NewCodeContextTool(settings sessionfs.NativeCodeContextSettings) (tools.Tool, error) {
	validator, err := tools.CompileSchema(codeContextInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native code_context schema: %w", err)
	}
	store := codeContextStore{dir: settings.StorageDir}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          codeContextToolID,
			Source:      "native",
			Name:        "code_context",
			Description: "Read and save exact source ranges with descriptions for a task, find them, and print their current code.",
			InputSchema: append(json.RawMessage(nil), codeContextInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"action":"get","task":"T-001"}`),
		NormalizeArguments: normalizeCodeContextArguments,
		Execute: func(_ context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeCodeContext(arguments, execution, settings, store), nil
		},
	}, nil
}

func normalizeCodeContextArguments(arguments map[string]any) map[string]any {
	normalized := make(map[string]any, len(arguments))
	for key, value := range arguments {
		normalized[key] = value
	}
	for _, key := range []string{"action", "task", "path", "since", "mode", "query", "from_task"} {
		if text, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(text)
		}
	}
	for _, key := range []string{"action", "since", "mode"} {
		if text, ok := normalized[key].(string); ok {
			normalized[key] = strings.ToLower(text)
		}
	}
	// A single id is accepted where ids is expected.
	if _, hasIDs := normalized["ids"]; !hasIDs {
		if action, _ := normalized["action"].(string); action == "drop" || action == "get" {
			if id, exists := normalized["id"]; exists {
				normalized["ids"] = []any{id}
				delete(normalized, "id")
			}
		}
	}
	return normalized
}

// codeContextCall is one call's arguments after validation.
type codeContextCall struct {
	action      string
	task        string
	id          int64
	ids         []int64
	path        string
	start, end  int64
	description string
	priority    int64
	since       string
	mode        string
	maxPriority int64
	query       string
	allTasks    bool
	fromTask    string
	has         map[string]bool
}

func parseCodeContextCall(arguments map[string]any) codeContextCall {
	call := codeContextCall{has: map[string]bool{}}
	call.action, _ = arguments["action"].(string)
	call.task, _ = arguments["task"].(string)
	if text, ok := arguments["path"].(string); ok {
		call.path, call.has["path"] = text, true
	}
	if text, ok := arguments["description"].(string); ok {
		call.description, call.has["description"] = normalizeCodeContextDescription(text), true
	}
	for key, target := range map[string]*string{"since": &call.since, "mode": &call.mode, "query": &call.query, "from_task": &call.fromTask} {
		if text, ok := arguments[key].(string); ok {
			*target, call.has[key] = text, true
		}
	}
	call.allTasks, _ = arguments["all_tasks"].(bool)
	for key, target := range map[string]*int64{"id": &call.id, "start": &call.start, "end": &call.end, "priority": &call.priority, "max_priority": &call.maxPriority} {
		if value, ok := safeInteger(arguments[key]); ok {
			*target, call.has[key] = value, true
		}
	}
	if items, ok := arguments["ids"].([]any); ok {
		for _, item := range items {
			if value, ok := safeInteger(item); ok && !slices.Contains(call.ids, value) {
				call.ids = append(call.ids, value)
			}
		}
	}
	return call
}

// codeContextCaller names who wrote an entry: the sub-agent, else the session's profile.
func codeContextCaller(execution tools.ExecutionContext) string {
	if execution.AgentName != "" {
		return execution.AgentName
	}
	if _, profile, found := strings.Cut(execution.SessionKey, "\x00"); found && profile != "" {
		return profile
	}
	return "session"
}

func executeCodeContext(arguments map[string]any, execution tools.ExecutionContext, settings sessionfs.NativeCodeContextSettings, store codeContextStore) string {
	call := parseCodeContextCall(arguments)
	callID := execution.CallID
	if !codeContextTaskPattern.MatchString(call.task) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_task", "task must be a plain name such as T-023")
	}
	if call.has["description"] && (call.description == "" || int64(utf8.RuneCountInString(call.description)) > settings.MaxDescriptionChars) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_description", fmt.Sprintf("description is empty or longer than %d characters", settings.MaxDescriptionChars))
	}
	for _, key := range []string{"priority", "max_priority"} {
		if value := map[string]int64{"priority": call.priority, "max_priority": call.maxPriority}[key]; call.has[key] && (value < 1 || value > 6) {
			return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_priority", key+" must be 1 to 6")
		}
	}
	if call.has["from_task"] && (!codeContextTaskPattern.MatchString(call.fromTask) || call.fromTask == call.task) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_from_task", "from_task must name another task")
	}
	reader := codeContextReader{settings: settings, execution: execution}
	by := codeContextCaller(execution)
	switch call.action {
	case "read":
		return codeContextReadRange(call, reader, store, by)
	case "add":
		if call.has["from_task"] {
			return codeContextAddFrom(call, reader, store, by)
		}
		return codeContextAdd(call, reader, store, by, "")
	case "update":
		return codeContextUpdate(call, reader, store, by)
	case "drop":
		return codeContextDrop(call, callID, store, settings, by)
	case "get":
		return codeContextGet(call, reader, store, by)
	case "list":
		return codeContextList(call, reader, store, by)
	case "find":
		return codeContextFind(call, reader, store)
	case "clear":
		removed, err := store.clear(call.task)
		if err != nil {
			return codeContextStoreFailure(callID, err)
		}
		if !removed {
			return fmt.Sprintf("code_context task %s has no entries; nothing to clear\n", call.task)
		}
		return fmt.Sprintf("code_context task %s cleared\n", call.task)
	}
	return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_action", "action must be read, add, update, drop, get, list, find or clear")
}

func codeContextStoreFailure(callID string, err error) string {
	var storeErr *codeContextStoreError
	if errors.As(err, &storeErr) {
		return tools.ExecutionError(callID, tools.StatusError, "code_context_storage", storeErr.message)
	}
	var failure *codeContextFailure
	if errors.As(err, &failure) {
		return failure.text
	}
	return tools.ExecutionError(callID, tools.StatusError, "code_context_failed", oneLine(err.Error()))
}

// codeContextFailure carries a rendered tool error out of a store operation.
type codeContextFailure struct{ text string }

func (f *codeContextFailure) Error() string { return "code_context call refused" }

// codeContextAdd saves call's range; note, when not empty, heads the answer.
func codeContextAdd(call codeContextCall, reader codeContextReader, store codeContextStore, by, note string) string {
	callID := reader.execution.CallID
	read, failure := reader.readRange(call.path, call.start, call.end)
	if failure != "" {
		return failure
	}
	var saved codeContextEntry
	duplicate := false
	document, err := store.mutate(call.task, by, reader.settings.MaxStorageBytes, func(document *codeContextDocument) error {
		for _, entry := range document.Entries {
			if entry.Path == read.path && entry.Start == call.start && entry.End == call.end {
				saved, duplicate = entry, true
				return errCodeContextNoChange
			}
		}
		if int64(len(document.Entries)) >= reader.settings.MaxEntries {
			return &codeContextFailure{tools.ExecutionError(callID, tools.StatusInvalidInput, "too_many_entries", fmt.Sprintf("task %s already has %d entries; max_entries is %d", call.task, len(document.Entries), reader.settings.MaxEntries))}
		}
		saved = codeContextEntry{
			ID: document.NextID, Priority: call.priority, Path: read.path, Start: call.start, End: call.end, Description: call.description,
			UpdatedBy: by, UpdatedAt: codeContextTimestamp(), Generation: document.Generation,
		}
		read.store(&saved)
		document.NextID++
		document.Entries = append(document.Entries, saved)
		return nil
	})
	if duplicate {
		current, loadErr := store.load(call.task)
		if loadErr != nil {
			return codeContextStoreFailure(callID, loadErr)
		}
		return "# the same range is already saved; nothing was added\n" + renderCodeContextYAML(current, []codeContextEntry{saved}, map[int64]codeContextStatus{saved.ID: {}}, nil)
	}
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}
	if note != "" {
		note = "# " + note + "\n"
	}
	return note + renderCodeContextYAML(document, []codeContextEntry{saved}, map[int64]codeContextStatus{saved.ID: {}}, nil)
}

// errCodeContextNoChange ends a mutation without writing.
var errCodeContextNoChange = errors.New("no change")

func codeContextUpdate(call codeContextCall, reader codeContextReader, store codeContextStore, by string) string {
	callID := reader.execution.CallID
	var saved codeContextEntry
	document, err := store.mutate(call.task, by, reader.settings.MaxStorageBytes, func(document *codeContextDocument) error {
		index := slices.IndexFunc(document.Entries, func(entry codeContextEntry) bool { return entry.ID == call.id })
		if index < 0 {
			return &codeContextFailure{tools.ExecutionError(callID, tools.StatusNotFound, "entry_not_found", fmt.Sprintf("task %s has no entry %d", call.task, call.id))}
		}
		entry := document.Entries[index]
		path, start, end := entry.Path, entry.Start, entry.End
		if call.has["path"] {
			path = call.path
		}
		if call.has["start"] {
			start = call.start
		}
		if call.has["end"] {
			end = call.end
		}
		read, failure := reader.readRange(path, start, end)
		if failure != "" {
			return &codeContextFailure{failure}
		}
		for _, other := range document.Entries {
			if other.ID != entry.ID && other.Path == read.path && other.Start == start && other.End == end {
				return &codeContextFailure{tools.ExecutionError(callID, tools.StatusInvalidInput, "duplicate_entry", fmt.Sprintf("entry %d already reads this range; drop one of them", other.ID))}
			}
		}
		entry.Path, entry.Start, entry.End = read.path, start, end
		read.store(&entry)
		if call.has["description"] {
			entry.Description = call.description
		}
		if call.has["priority"] {
			entry.Priority = call.priority
		}
		entry.UpdatedBy, entry.UpdatedAt, entry.Generation = by, codeContextTimestamp(), document.Generation
		document.Entries[index] = entry
		saved = entry
		return nil
	})
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}
	return renderCodeContextYAML(document, []codeContextEntry{saved}, map[int64]codeContextStatus{saved.ID: {}}, nil)
}

func codeContextDrop(call codeContextCall, callID string, store codeContextStore, settings sessionfs.NativeCodeContextSettings, by string) string {
	var dropped, missing []int64
	document, err := store.mutate(call.task, by, settings.MaxStorageBytes, func(document *codeContextDocument) error {
		kept := document.Entries[:0]
		for _, entry := range document.Entries {
			if slices.Contains(call.ids, entry.ID) {
				dropped = append(dropped, entry.ID)
				continue
			}
			kept = append(kept, entry)
		}
		document.Entries = kept
		for _, id := range call.ids {
			if !slices.Contains(dropped, id) {
				missing = append(missing, id)
			}
		}
		if len(dropped) == 0 {
			return errCodeContextNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, errCodeContextNoChange) {
		return codeContextStoreFailure(callID, err)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "code_context task %s", call.task)
	if len(dropped) > 0 {
		fmt.Fprintf(&output, " generation %d: dropped %s", document.Generation, joinCodeContextIDs(dropped))
	} else {
		output.WriteString(": nothing dropped")
	}
	if len(missing) > 0 {
		fmt.Fprintf(&output, "; no entry %s", joinCodeContextIDs(missing))
	}
	output.WriteString("\n")
	return output.String()
}

func joinCodeContextIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for index, id := range ids {
		parts[index] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

// codeContextReader reads ranges under the tool's path rules and size limits.
type codeContextReader struct {
	settings  sessionfs.NativeCodeContextSettings
	execution tools.ExecutionContext
}

type codeContextRead struct {
	// path is the stored form: relative to the workspace when inside it.
	path          string
	data          []byte
	content       []byte
	fileSHA256    string
	contentSHA256 string
}

// store sets the hashes of what was read on entry.
func (r codeContextRead) store(entry *codeContextEntry) {
	entry.FileSHA256, entry.ContentSHA256 = r.fileSHA256, r.contentSHA256
	entry.ContentBytes, entry.HeadSHA256 = int64(len(r.content)), codeContextSHA256(codeContextHead(r.content))
}

// readFile reads the whole file the path names. A failure is a rendered tool error, and
// reason is the short form a stale entry reports.
func (r codeContextReader) readFile(path string) (data []byte, display string, failure string, reason string) {
	callID := r.execution.CallID
	target, failure := resolveExistingTarget(path, r.execution.WorkspaceRoot, scopedPolicy(r.settings.Policy, r.execution), callID)
	if failure != "" {
		if strings.Contains(failure, "error_code: not_found") {
			return nil, "", failure, "the file no longer exists"
		}
		return nil, "", failure, "the file can no longer be read here"
	}
	info, err := os.Lstat(target.realPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", tools.ExecutionError(callID, tools.StatusNotFound, "not_found", "Could not find "+target.errorPath), "the file no longer exists"
		}
		return nil, "", tools.ExecutionError(callID, tools.StatusError, "read_failed", target.errorPath+" cannot be read"), "the file cannot be read"
	}
	if !info.Mode().IsRegular() {
		return nil, "", tools.ExecutionError(callID, tools.StatusInvalidInput, "not_a_file", target.errorPath+" is not a regular file"), "the path is no longer a regular file"
	}
	tooLarge := tools.ExecutionError(callID, tools.StatusInvalidInput, "file_too_large", fmt.Sprintf("%s is larger than max_file_bytes (%d)", target.errorPath, r.settings.MaxFileBytes))
	if info.Size() > r.settings.MaxFileBytes {
		return nil, "", tooLarge, "the file is now larger than max_file_bytes"
	}
	data, _, err = readVerifiedRegularFile(target.realPath, info, r.settings.MaxFileBytes)
	if err != nil {
		if strings.Contains(err.Error(), "past configured limit") {
			return nil, "", tooLarge, "the file is now larger than max_file_bytes"
		}
		return nil, "", tools.ExecutionError(callID, tools.StatusError, "read_failed", target.errorPath+" cannot be read: "+oneLine(err.Error())), "the file cannot be read"
	}
	return data, target.displayPath, "", ""
}

// readRange reads lines start..end of path for add and update, refusing a range the
// file does not hold.
func (r codeContextReader) readRange(path string, start, end int64) (codeContextRead, string) {
	callID := r.execution.CallID
	if start < 1 || end < start {
		return codeContextRead{}, tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_range", "start must be at least 1 and end at least start")
	}
	if end-start+1 > r.settings.MaxRangeLines {
		return codeContextRead{}, tools.ExecutionError(callID, tools.StatusInvalidInput, "range_too_long", fmt.Sprintf("the range has %d lines; max_range_lines is %d", end-start+1, r.settings.MaxRangeLines))
	}
	data, display, failure, _ := r.readFile(path)
	if failure != "" {
		return codeContextRead{}, failure
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return codeContextRead{}, tools.ExecutionError(callID, tools.StatusInvalidInput, "not_text", display+" is not a text file")
	}
	content, lines, ok := codeContextLines(data, start, end)
	if !ok {
		return codeContextRead{}, tools.ExecutionError(callID, tools.StatusInvalidInput, "range_outside_file", fmt.Sprintf("%s has %d lines; the range is %d-%d", display, lines, start, end))
	}
	return codeContextRead{path: display, data: data, content: content, fileSHA256: codeContextSHA256(data), contentSHA256: codeContextSHA256(content)}, ""
}

// codeContextLines returns the exact bytes of lines start..end (1-based, inclusive),
// line endings included, and the file's line count. A final line without a newline is
// a line; a trailing newline does not start another.
func codeContextLines(data []byte, start, end int64) ([]byte, int64, bool) {
	var line int64 = 1
	from, to := -1, -1
	if start == 1 {
		from = 0
	}
	for index, char := range data {
		if char != '\n' {
			continue
		}
		if line == end {
			to = index + 1
		}
		line++
		if line == start {
			from = index + 1
		}
	}
	lines := line - 1
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines = line
		if end == line {
			to = len(data)
		}
	}
	if from < 0 || to < 0 || end > lines || from > to {
		return nil, lines, false
	}
	return data[from:to], lines, true
}

func codeContextSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// yamlString quotes a scalar as a JSON string, which YAML reads as a double-quoted one.
func yamlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func indentCodeContext(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if line != "" {
			lines[index] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// codeContextToken marks where code starts and ends, so code that contains a marker-like
// line cannot end a block early.
func codeContextToken() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(raw[:])
}
