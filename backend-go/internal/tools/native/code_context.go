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
    "action":{"type":"string","enum":["read","update","drop","get","list","find","clear"],"description":"read prints lines and saves them; with a description it saves them without printing. update changes an entry by id. drop removes ids. get prints the saved code. list shows the entries, or without task the tasks (the default). find searches descriptions and paths. clear deletes the task."},
    "task":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$","description":"Task key, e.g. T-023."},
    "task_description":{"type":"string","minLength":1,"description":"read, update: what the task is about."},
    "id":{"type":"integer","minimum":1,"description":"update: the entry."},
    "ids":{"type":"array","items":{"anyOf":[{"type":"integer","minimum":1},{"type":"string","pattern":"^[1-9][0-9]*(-[1-9][0-9]*)?$"}]},"minItems":1,"description":"drop, get: entry ids, or ranges of ids such as \"1-20\"; get without ids prints all."},
    "path":{"type":"string","minLength":1,"description":"File, relative to cwd."},
    "start":{"type":"integer","minimum":1,"description":"First line, 1-based."},
    "end":{"type":"integer","minimum":1,"description":"Last line, inclusive."},
    "description":{"type":"string","minLength":1,"description":"What the code does and why it matters for the task; no line numbers."},
    "priority":{"type":"integer","minimum":1,"maximum":6,"description":"1 target, 2 callers/callees, 3 interfaces, 4 data, 5 tests, 6 other. get leaves out high numbers first."},
    "query":{"type":"string","minLength":1,"description":"find: words to look for."},
    "items":{"type":"array","minItems":1,"description":"read, update: several in one call; fields outside items are their defaults.","items":{"type":"object","properties":{
      "id":{"type":"integer","minimum":1},
      "path":{"type":"string","minLength":1},
      "start":{"type":"integer","minimum":1},
      "end":{"type":"integer","minimum":1},
      "description":{"type":"string","minLength":1},
      "priority":{"type":"integer","minimum":1,"maximum":6}
    },"additionalProperties":false}}
  },
  "additionalProperties":false
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
			Description: "Save exact source ranges with descriptions per task, find them, and print their current code.",
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
	for _, key := range []string{"action", "task", "path", "query"} {
		if text, ok := normalized[key].(string); ok {
			normalized[key] = strings.TrimSpace(text)
		}
	}
	// A call without an action lists: the tasks, or the entries of the task named.
	action, _ := normalized["action"].(string)
	action = strings.ToLower(action)
	if action == "" {
		action = "list"
	}
	normalized["action"] = action
	if items, ok := normalized["items"].([]any); ok {
		copied := make([]any, len(items))
		for index, item := range items {
			copied[index] = item
			if fields, ok := item.(map[string]any); ok {
				trimmed := make(map[string]any, len(fields))
				for key, value := range fields {
					if text, ok := value.(string); ok && key == "path" {
						value = strings.TrimSpace(text)
					}
					trimmed[key] = value
				}
				copied[index] = trimmed
			}
		}
		normalized["items"] = copied
		// drop and get take ids: items that only name an id become those ids.
		if action == "drop" || action == "get" {
			if ids, only := codeContextItemIDs(copied); only {
				if _, hasIDs := normalized["ids"]; !hasIDs {
					normalized["ids"] = ids
					delete(normalized, "items")
				}
			}
		}
	}
	// A single id, or a single range, is accepted where ids is expected.
	if _, hasIDs := normalized["ids"]; !hasIDs && (action == "drop" || action == "get") {
		if id, exists := normalized["id"]; exists {
			normalized["ids"] = []any{id}
			delete(normalized, "id")
		}
	}
	if text, ok := normalized["ids"].(string); ok {
		normalized["ids"] = []any{text}
	}
	if ids, ok := normalized["ids"].([]any); ok {
		copied := make([]any, len(ids))
		for index, id := range ids {
			if text, ok := id.(string); ok {
				id = strings.Join(strings.Fields(text), "")
			}
			copied[index] = id
		}
		normalized["ids"] = copied
	}
	return normalized
}

// codeContextItemIDs answers the ids of items that each hold only an id.
func codeContextItemIDs(items []any) ([]any, bool) {
	ids := make([]any, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		id, hasID := fields["id"]
		if !ok || !hasID || len(fields) != 1 {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, len(ids) > 0
}

// codeContextCall is one call's arguments, or one item's with the call's as defaults.
type codeContextCall struct {
	action string
	task   string
	id     int64
	ids    []int64
	// idRanges are the ranges of ids, first and last included, ids gives as "a-b".
	idRanges [][2]int64
	// badIDs are the ids and ranges of ids that are not whole numbers an entry id can be
	// (one too large, say); the call is refused, never read as one without them.
	badIDs      []string
	path        string
	start, end  int64
	description string
	priority    int64
	query       string
	// taskDescription describes the task itself, not an entry.
	taskDescription string
	// items are the item calls of a read or update, nil without items.
	items []codeContextCall
	has   map[string]bool
}

func parseCodeContextCall(arguments map[string]any) codeContextCall {
	call := parseCodeContextFields(arguments, nil)
	if items, ok := arguments["items"].([]any); ok {
		call.has["items"] = true
		call.items = make([]codeContextCall, len(items))
		for index, item := range items {
			fields, _ := item.(map[string]any)
			call.items[index] = parseCodeContextFields(fields, &call)
		}
	}
	return call
}

// parseCodeContextFields reads one call's fields; with base, a field the arguments leave
// out is base's.
func parseCodeContextFields(arguments map[string]any, base *codeContextCall) codeContextCall {
	call := codeContextCall{has: map[string]bool{}}
	if base != nil {
		call = *base
		call.items, call.has = nil, map[string]bool{}
		for key, value := range base.has {
			if key != "items" {
				call.has[key] = value
			}
		}
	} else {
		call.action, _ = arguments["action"].(string)
		call.task, _ = arguments["task"].(string)
		if text, ok := arguments["task_description"].(string); ok {
			call.taskDescription, call.has["task_description"] = normalizeCodeContextDescription(text), true
		}
		if text, ok := arguments["query"].(string); ok {
			call.query, call.has["query"] = text, true
		}
		if items, ok := arguments["ids"].([]any); ok {
			for _, item := range items {
				value, ok := safeInteger(item)
				if text, isText := item.(string); isText {
					first, last, isRange := strings.Cut(text, "-")
					if isRange {
						from, fromErr := strconv.ParseInt(first, 10, 64)
						to, toErr := strconv.ParseInt(last, 10, 64)
						if fromErr != nil || toErr != nil {
							call.badIDs = append(call.badIDs, text)
						} else if span := [2]int64{from, to}; !slices.Contains(call.idRanges, span) {
							call.idRanges = append(call.idRanges, span)
						}
						continue
					}
					parsed, err := strconv.ParseInt(text, 10, 64)
					value, ok = parsed, err == nil
				}
				switch {
				case !ok || value < 1:
					call.badIDs = append(call.badIDs, fmt.Sprint(item))
				case !slices.Contains(call.ids, value):
					call.ids = append(call.ids, value)
				}
			}
			call.has["ids"] = len(call.ids) > 0 || len(call.idRanges) > 0 || len(call.badIDs) > 0
		}
	}
	if text, ok := arguments["path"].(string); ok {
		call.path, call.has["path"] = text, true
	}
	if text, ok := arguments["description"].(string); ok {
		call.description, call.has["description"] = normalizeCodeContextDescription(text), true
	}
	for key, target := range map[string]*int64{"id": &call.id, "start": &call.start, "end": &call.end, "priority": &call.priority} {
		if value, ok := safeInteger(arguments[key]); ok {
			*target, call.has[key] = value, true
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
	if failure := validateCodeContextCall(call, settings, callID); failure != "" {
		return failure
	}
	reader := codeContextReader{settings: settings, execution: execution}
	by := codeContextCaller(execution)
	switch call.action {
	case "list":
		if call.task == "" {
			return codeContextTasks(store, settings, callID)
		}
		return codeContextList(call, reader, store, by)
	case "read", "update":
		return codeContextWrite(call, reader, store, by)
	case "drop":
		return codeContextDrop(call, callID, store, settings, by)
	case "get":
		return codeContextGet(call, reader, store, by)
	case "find":
		return codeContextFind(call, reader, store, by)
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
	return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_action", "action must be read, update, drop, get, list, find or clear")
}

// validateCodeContextCall checks what each action requires, the task, the lengths of the
// descriptions and the priorities. A failure is a rendered tool error.
func validateCodeContextCall(call codeContextCall, settings sessionfs.NativeCodeContextSettings, callID string) string {
	invalid := func(code, summary string) string {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, code, summary)
	}
	if !codeContextTaskPattern.MatchString(call.task) && (call.task != "" || call.action != "list") {
		return invalid("invalid_task", "task must be a plain name such as T-023")
	}
	writes := call.action == "read" || call.action == "update"
	for _, key := range []string{"task_description", "items"} {
		if call.has[key] && !writes {
			return invalid("invalid_field", key+" is taken by read and update only")
		}
	}
	if int64(len(call.items)) > settings.MaxBatchItems {
		return invalid("too_many_items", fmt.Sprintf("the call has %d items; max_batch_items is %d", len(call.items), settings.MaxBatchItems))
	}
	switch {
	case call.action == "drop" && !call.has["ids"]:
		return invalid("missing_fields", "drop needs ids")
	case call.action == "find" && !call.has["query"]:
		return invalid("missing_fields", "find needs query")
	case call.action == "update" && !call.has["items"] && !call.has["id"] && !call.has["task_description"]:
		return invalid("missing_fields", "update needs id, items or task_description")
	}
	if len(call.badIDs) > 0 {
		return invalid("invalid_id", fmt.Sprintf("ids holds %s, which is not an entry id; ids are whole numbers from 1, or ranges such as \"1-20\"", call.badIDs[0]))
	}
	for _, span := range call.idRanges {
		if span[0] < 1 || span[1] < span[0] {
			return invalid("invalid_range", fmt.Sprintf("the id range %d-%d is not from a smaller id to a larger one", span[0], span[1]))
		}
	}
	if call.has["task_description"] && !validCodeContextDescription(call.taskDescription, settings) {
		return invalid("invalid_description", fmt.Sprintf("task_description is empty or longer than %d characters", settings.MaxDescriptionChars))
	}
	return validateCodeContextItem(call, settings, callID)
}

// validateCodeContextItem checks the fields of one read or update.
func validateCodeContextItem(call codeContextCall, settings sessionfs.NativeCodeContextSettings, callID string) string {
	if call.has["description"] && !validCodeContextDescription(call.description, settings) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_description", fmt.Sprintf("description is empty or longer than %d characters", settings.MaxDescriptionChars))
	}
	if call.has["priority"] && (call.priority < 1 || call.priority > 6) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "invalid_priority", "priority must be 1 to 6")
	}
	return ""
}

func validCodeContextDescription(text string, settings sessionfs.NativeCodeContextSettings) bool {
	return text != "" && int64(utf8.RuneCountInString(text)) <= settings.MaxDescriptionChars
}

// codeContextMissingFields names what a read or update still lacks.
func codeContextMissingFields(call codeContextCall) string {
	switch {
	case call.action == "read" && !(call.has["path"] && call.has["start"] && call.has["end"]):
		return "read needs path, start and end"
	case call.action == "update" && !call.has["id"]:
		return "update needs id"
	}
	return ""
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

// errCodeContextNoChange ends a mutation without writing.
var errCodeContextNoChange = errors.New("no change")

func codeContextDrop(call codeContextCall, callID string, store codeContextStore, settings sessionfs.NativeCodeContextSettings, by string) string {
	var dropped []int64
	var missing []string
	document, err := store.mutate(call.task, by, settings.MaxStorageBytes, func(document *codeContextDocument) error {
		kept := document.Entries[:0]
		for _, entry := range document.Entries {
			if call.selects(entry.ID) {
				dropped = append(dropped, entry.ID)
				continue
			}
			kept = append(kept, entry)
		}
		document.Entries = kept
		missing = call.unmatched(dropped)
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
		fmt.Fprintf(&output, "; no entry %s", strings.Join(missing, ", "))
	}
	output.WriteString("\n")
	return output.String()
}

// selects reports whether the call's ids or id ranges name id.
func (c codeContextCall) selects(id int64) bool {
	if slices.Contains(c.ids, id) {
		return true
	}
	return slices.ContainsFunc(c.idRanges, func(span [2]int64) bool { return span[0] <= id && id <= span[1] })
}

// unmatched names the ids, and the id ranges, of the call that found holds none of:
// "9", "30-40".
func (c codeContextCall) unmatched(found []int64) []string {
	var missing []string
	for _, id := range c.ids {
		if !slices.Contains(found, id) {
			missing = append(missing, strconv.FormatInt(id, 10))
		}
	}
	for _, span := range c.idRanges {
		if !slices.ContainsFunc(found, func(id int64) bool { return span[0] <= id && id <= span[1] }) {
			missing = append(missing, fmt.Sprintf("%d-%d", span[0], span[1]))
		}
	}
	return missing
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
	info, err := os.Lstat(target.openPath)
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
	data, _, err = readVerifiedTarget(target, info, r.settings.MaxFileBytes)
	if err != nil {
		if strings.Contains(err.Error(), "past configured limit") {
			return nil, "", tooLarge, "the file is now larger than max_file_bytes"
		}
		return nil, "", tools.ExecutionError(callID, tools.StatusError, "read_failed", target.errorPath+" cannot be read: "+oneLine(err.Error())), "the file cannot be read"
	}
	return data, target.displayPath, "", ""
}

// readRange reads lines start..end of path for read and update, refusing a range the
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
