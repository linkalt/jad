package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const taskToolID = "native.task"

const taskDescription = "Create and maintain session tasks. Pass ops to run several actions in one call."

var taskInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["list","get","add","update","done","delete"],"description":"Inferred when omitted."},
    "id":{"type":"string","description":"Task id, e.g. T-001."},
    "title":{"type":"string","minLength":1},
    "status":{"type":"string","enum":["todo","in-progress","blocked","done","all"],"description":"all is a list filter. List default: not done."},
    "priority":{"type":"string","enum":["low","normal","high","urgent"],"description":"Default normal on add."},
    "tags":{"type":"array","items":{"type":"string"},"description":"[] clears on update."},
    "documents":{"type":"array","items":{"type":"string"},"description":"Planning-document paths; [] clears."},
    "tag":{"type":"string","description":"List filter."},
    "q":{"type":"string","description":"List: words found in title, tags or document paths."},
    "limit":{"type":"integer","minimum":1},
    "offset":{"type":"integer","minimum":0,"description":"List: skip this many."},
    "detail":{"type":"boolean","description":"List: add document paths and update time."},
    "ops":{"type":"array","minItems":1,"items":{"type":"object"},"description":"Batch instead of the fields above: objects with the same fields, run in order, all or nothing."}
  },
  "additionalProperties":false
}`)

type taskAction string

const (
	taskActionList   taskAction = "list"
	taskActionGet    taskAction = "get"
	taskActionAdd    taskAction = "add"
	taskActionUpdate taskAction = "update"
	taskActionDone   taskAction = "done"
	taskActionDelete taskAction = "delete"
)

var taskActionAliases = map[string]taskAction{
	"list":     taskActionList,
	"get":      taskActionGet,
	"add":      taskActionAdd,
	"create":   taskActionAdd,
	"new":      taskActionAdd,
	"update":   taskActionUpdate,
	"edit":     taskActionUpdate,
	"done":     taskActionDone,
	"complete": taskActionDone,
	"finish":   taskActionDone,
	"delete":   taskActionDelete,
	"remove":   taskActionDelete,
}

var taskStatusSeparator = regexp.MustCompile(`[ _]+`)
var taskIDTextPattern = regexp.MustCompile(`^T-[0-9]{3,}$`)

// NewTaskTool binds task policy to a persistent task store.
func NewTaskTool(settings sessionfs.NativeTaskSettings, store *TaskStore) (tools.Tool, error) {
	validator, err := tools.CompileSchema(taskInputSchema)
	if err != nil {
		return tools.Tool{}, fmt.Errorf("compile native task schema: %w", err)
	}
	if store == nil {
		store = NewTaskStore(settings.StorageDir)
	}
	return tools.Tool{
		Definition: tools.Definition{
			ID:          taskToolID,
			Source:      "native",
			Name:        "task",
			Description: taskDescription,
			InputSchema: append(json.RawMessage(nil), taskInputSchema...),
		},
		Validator:          validator,
		ExampleOperation:   json.RawMessage(`{"title":"Plan the next change"}`),
		NormalizeArguments: normalizeTaskArguments,
		Execute: func(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext) (string, error) {
			return executeNativeTask(ctx, arguments, execution, settings, store)
		},
	}, nil
}

func normalizeTaskArguments(arguments map[string]any) map[string]any {
	_, batch := arguments["ops"]
	normalized := normalizeTaskFields(arguments, !batch)
	if items, ok := normalized["ops"].([]any); ok {
		// Each op is a call of its own: it is normalized the way a flat call is, so an
		// alias or an omitted action means the same in a batch.
		ops := make([]any, len(items))
		for index, item := range items {
			ops[index] = item
			if fields, ok := item.(map[string]any); ok {
				ops[index] = normalizeTaskFields(fields, true)
			}
		}
		normalized["ops"] = ops
	}
	return normalized
}

// normalizeTaskID turns the spellings of a task number a model may write (1, T-1, t1,
// T-001) into the canonical T-001. Anything else is only trimmed and upper-cased, so a
// value that is not a positive task number stays invalid for the task action to refuse.
// Normalizing twice gives the same id, because %03d of a number reads back as that number.
func normalizeTaskID(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	digits := id
	if strings.HasPrefix(id, "T") {
		digits = strings.TrimPrefix(strings.TrimPrefix(id, "T"), "-")
	}
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return id
	}
	number, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || number < 1 || number > maxSafeTaskID {
		return id
	}
	return fmt.Sprintf("T-%03d", number)
}

// normalizeTaskFields normalizes the fields of one call or of one op. A call that has
// ops infers no action, so ops stays the only field of a valid batch.
func normalizeTaskFields(arguments map[string]any, inferAction bool) map[string]any {
	normalized := make(map[string]any, len(arguments)+1)
	for key, value := range arguments {
		normalized[key] = value
	}
	if _, hasID := normalized["id"]; !hasID {
		if taskID, exists := arguments["taskId"]; exists {
			normalized["id"] = taskID
		}
	}
	if _, hasLimit := normalized["limit"]; !hasLimit {
		if maxItems, exists := arguments["maxItems"]; exists {
			normalized["limit"] = maxItems
		}
	}
	delete(normalized, "taskId")
	delete(normalized, "maxItems")

	if action, ok := normalized["action"].(string); ok {
		raw := strings.ToLower(strings.TrimSpace(action))
		var compact strings.Builder
		for _, r := range raw {
			if r != ' ' && r != '_' && r != '-' {
				compact.WriteRune(r)
			}
		}
		key := compact.String()
		if mapped, exists := taskActionAliases[key]; exists {
			normalized["action"] = string(mapped)
		} else if key != "" {
			normalized["action"] = key
		}
	}
	if id, ok := normalized["id"].(string); ok {
		normalized["id"] = normalizeTaskID(id)
	}
	if query, ok := normalized["q"].(string); ok {
		normalized["q"] = strings.TrimSpace(query)
	}
	if title, ok := normalized["title"].(string); ok {
		normalized["title"] = strings.TrimSpace(title)
	}
	if status, ok := normalized["status"].(string); ok {
		status = strings.ToLower(strings.TrimSpace(status))
		status = taskStatusSeparator.ReplaceAllString(status, "-")
		switch status {
		case "inprogress":
			status = "in-progress"
		case "complete", "completed":
			status = "done"
		}
		normalized["status"] = status
	}
	if priority, ok := normalized["priority"].(string); ok {
		normalized["priority"] = strings.ToLower(strings.TrimSpace(priority))
	}
	if tag, ok := normalized["tag"].(string); ok {
		normalized["tag"] = strings.ToLower(strings.TrimSpace(tag))
	}
	if tags, exists := normalized["tags"]; exists {
		normalized["tags"] = normalizeTaskArray(tags, true)
	}
	if documents, exists := normalized["documents"]; exists {
		normalized["documents"] = normalizeTaskArray(documents, false)
	}
	if limit, ok := safeInteger(normalized["limit"]); ok {
		if limit < 1 {
			limit = 1
		}
		normalized["limit"] = json.Number(fmt.Sprint(limit))
	}
	if offset, ok := safeInteger(normalized["offset"]); ok {
		normalized["offset"] = json.Number(fmt.Sprint(max(offset, 0)))
	}
	if _, exists := normalized["action"]; !exists && inferAction {
		id, isString := normalized["id"].(string)
		hasID := isString && len(id) > 0
		_, hasTitle := normalized["title"]
		hasPatch := false
		for _, key := range []string{"title", "status", "priority", "tags", "documents"} {
			if _, exists := normalized[key]; exists {
				hasPatch = true
				break
			}
		}
		switch {
		case hasID && hasPatch:
			normalized["action"] = string(taskActionUpdate)
		case hasID:
			normalized["action"] = string(taskActionGet)
		case hasTitle:
			normalized["action"] = string(taskActionAdd)
		default:
			normalized["action"] = string(taskActionList)
		}
	}
	return normalized
}

func normalizeTaskArray(value any, lower bool) any {
	items, ok := value.([]any)
	if !ok {
		return value
	}
	seen := make(map[string]struct{}, len(items))
	result := make([]any, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			result = append(result, item)
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if lower {
			text = strings.ToLower(text)
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	return result
}

func executeNativeTask(
	ctx context.Context,
	arguments map[string]any,
	execution tools.ExecutionContext,
	settings sessionfs.NativeTaskSettings,
	store *TaskStore,
) (string, error) {
	_ = ctx
	if store == nil {
		return tools.ExecutionError(execution.CallID, tools.StatusError, "task_store_unavailable", "The task store is not available"), nil
	}
	if _, batch := arguments["ops"]; batch {
		return executeTaskBatch(arguments, execution, settings, store)
	}
	call, failure := parseTaskCall(arguments, settings)
	if failure != nil {
		return taskFailure(execution.CallID, failure.code, failure.summary), nil
	}
	op := call.op
	outcome, err := store.Apply([]TaskOp{op}, settings)
	if err != nil {
		return taskStorageError(execution.CallID, err), nil
	}
	if outcome.Miss != nil {
		return taskNotFound(execution.CallID, op.ID, outcome.Miss), nil
	}
	result := outcome.Results[0]
	switch op.Action {
	case taskActionList:
		return renderTaskList(execution.CallID, result.List, op.Filter.Offset, call.detail, settings)
	case taskActionAdd:
		return renderTask(execution.CallID, op.Action, result.Task, "Task added")
	case taskActionGet:
		return renderTask(execution.CallID, op.Action, result.Task, "Task retrieved")
	case taskActionDelete:
		return taskRender(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: "Task deleted", Information: []tools.Information{{Key: "action", Value: string(op.Action)}, {Key: "task_id", Value: op.ID}}})
	case taskActionDone:
		return renderTask(execution.CallID, op.Action, result.Task, "Task completed")
	default:
		return renderTask(execution.CallID, op.Action, result.Task, "Task updated")
	}
}

// taskCall is one validated call of the task tool, a flat call or one op of a batch.
type taskCall struct {
	op TaskOp
	// detail asks a list for document paths and update times.
	detail bool
}

// taskCallFailure is a rule a call breaks, as the error code and summary the model reads.
type taskCallFailure struct {
	code    string
	summary string
}

// parseTaskCall checks one call against every rule of its action and builds the store
// operation. The rules are not in the schema (a flat schema cannot say them for the ops
// of a batch), so this is the one place that enforces them.
func parseTaskCall(arguments map[string]any, settings sessionfs.NativeTaskSettings) (taskCall, *taskCallFailure) {
	fail := func(code, summary string) (taskCall, *taskCallFailure) {
		return taskCall{}, &taskCallFailure{code: code, summary: summary}
	}
	action, valid := taskActionFor(arguments)
	if !valid {
		return fail("invalid_action", "Task action is invalid")
	}
	id := taskText(arguments["id"])
	if id != "" {
		id = normalizeTaskID(id)
	}
	taskStatus, hasTaskStatus := taskStatusValue(arguments["status"])
	taskPriority, hasTaskPriority := taskPriorityValue(arguments["priority"])
	title, hasTitle := taskString(arguments["title"])
	if hasTitle {
		title = strings.TrimSpace(title)
	}
	tags, hasTags := taskStringArray(arguments["tags"], settings.MaxTags, settings.MaxTagChars)
	documents, hasDocuments := taskStringArray(arguments["documents"], settings.MaxDocuments, settings.MaxDocumentChars)

	if hasTitle && !validTaskText(title, settings.MaxTitleChars) {
		return fail("invalid_title", "Task title is empty or exceeds the configured length")
	}
	if _, exists := arguments["tags"]; exists && !hasTags {
		return fail("invalid_tags", "Task tags exceed the configured limits or contain empty values")
	}
	if _, exists := arguments["documents"]; exists && !hasDocuments {
		return fail("invalid_documents", "Task documents exceed the configured limits or contain empty values")
	}

	switch action {
	case taskActionList:
		if !taskNoExtra(arguments, "action", "status", "priority", "tag", "q", "limit", "offset", "detail") {
			return fail("invalid_list", "List accepts only status, priority, tag, q, limit, offset, and detail")
		}
		if _, exists := arguments["status"]; exists && arguments["status"] != "all" && !hasTaskStatus {
			return fail("invalid_status", "Task status is invalid")
		}
		if _, exists := arguments["priority"]; exists && !hasTaskPriority {
			return fail("invalid_priority", "Task priority is invalid")
		}
		query, hasQuery := arguments["q"]
		if _, isText := query.(string); hasQuery && !isText {
			return fail("invalid_list", "List q must be a string")
		}
		detail, hasDetail := arguments["detail"]
		if _, isBool := detail.(bool); hasDetail && !isBool {
			return fail("invalid_list", "List detail must be true or false")
		}
		offset := int64(0)
		if raw, exists := arguments["offset"]; exists {
			value, ok := safeInteger(raw)
			if !ok || value < 0 {
				return fail("invalid_list", "List offset must be a whole number of 0 or more")
			}
			offset = value
		}
		limit := int(settings.ListLimit)
		if requested, ok := safeInteger(arguments["limit"]); ok {
			limit = int(requested)
		}
		if limit < 1 {
			limit = 1
		}
		if int64(limit) > settings.MaxListLimit {
			limit = int(settings.MaxListLimit)
		}
		filter := TaskListFilter{Limit: limit, Offset: int(min(offset, math.MaxInt32))}
		if arguments["status"] == "all" {
			filter.Status = "all"
		} else if hasTaskStatus {
			filter.Status = string(taskStatus)
		}
		if hasTaskPriority {
			filter.Priority = taskPriority
		}
		if tag, ok := arguments["tag"].(string); ok {
			filter.Tag = tag
		}
		if text, ok := query.(string); ok {
			filter.Query = text
		}
		return taskCall{op: TaskOp{Action: action, Filter: filter}, detail: detail == true}, nil

	case taskActionAdd:
		if !taskNoExtra(arguments, "action", "title", "status", "priority", "tags", "documents") {
			return fail("invalid_add", "Add accepts title, todo or in-progress status, priority, tags, and documents")
		}
		if !hasTitle || title == "" {
			return fail("missing_title", "Add requires a title")
		}
		if _, exists := arguments["status"]; exists && (!hasTaskStatus || (taskStatus != TaskTodo && taskStatus != TaskInProgress)) {
			return fail("invalid_creation_status", "New tasks must be todo or in-progress")
		}
		if _, exists := arguments["priority"]; exists && !hasTaskPriority {
			return fail("invalid_priority", "Task priority is invalid")
		}
		if !hasTaskStatus {
			taskStatus = TaskTodo
		}
		if !hasTaskPriority {
			taskPriority = TaskPriorityNormal
		}
		if !hasTags {
			tags = []string{}
		}
		if !hasDocuments {
			documents = []string{}
		}
		return taskCall{op: TaskOp{Action: action, Input: TaskInput{Title: title, Status: taskStatus, Priority: taskPriority, Tags: tags, Documents: documents}}}, nil

	default:
		if id == "" {
			return fail("missing_task_id", "This task action requires an id")
		}
		if !taskIDTextPattern.MatchString(id) {
			return fail("invalid_task_id", "Task id is invalid")
		}
		switch action {
		case taskActionGet:
			if !taskNoExtra(arguments, "action", "id") {
				return fail("invalid_get", "Get accepts only an id")
			}
			return taskCall{op: TaskOp{Action: action, ID: id}}, nil

		case taskActionDelete:
			if !taskNoExtra(arguments, "action", "id") {
				return fail("invalid_delete", "Delete requires exactly an id")
			}
			return taskCall{op: TaskOp{Action: action, ID: id}}, nil

		case taskActionDone:
			if !taskNoExtra(arguments, "action", "id") {
				return fail("invalid_done", "Done requires exactly an id")
			}
			return taskCall{op: TaskOp{Action: action, ID: id}}, nil

		case taskActionUpdate:
			if !taskNoExtra(arguments, "action", "id", "title", "status", "priority", "tags", "documents") {
				return fail("invalid_update", "Update accepts title, status, priority, tags, and documents")
			}
			if _, exists := arguments["status"]; exists && !hasTaskStatus {
				return fail("invalid_status", "Task status is invalid")
			}
			if _, exists := arguments["priority"]; exists && !hasTaskPriority {
				return fail("invalid_priority", "Task priority is invalid")
			}
			patch := TaskPatch{}
			if hasTitle {
				patch.Title = &title
			}
			if hasTaskStatus {
				patch.Status = &taskStatus
			}
			if hasTaskPriority {
				patch.Priority = &taskPriority
			}
			if hasTags {
				patch.Tags = &tags
			}
			if hasDocuments {
				patch.Documents = &documents
			}
			if !hasTitle && !hasTaskStatus && !hasTaskPriority && !hasTags && !hasDocuments {
				return fail("missing_update", "Update needs a title, status, priority, tags, or documents")
			}
			return taskCall{op: TaskOp{Action: action, ID: id, Patch: patch}}, nil
		}
	}
	return fail("invalid_action", "Task action is invalid")
}

// taskListLine is the line a task has in a list: id, status, priority and title, then
// its tags and its documents. detail names the documents instead of counting them and
// adds the update time.
func taskListLine(task TaskRecord, detail bool) string {
	line := fmt.Sprintf("%s | %s | %s | %s", task.ID, task.Status, task.Priority, task.Title)
	if len(task.Tags) > 0 {
		line += " | tags: " + strings.Join(task.Tags, ",")
	}
	if len(task.Documents) > 0 {
		if detail {
			line += " | documents: " + strings.Join(task.Documents, ",")
		} else {
			line += fmt.Sprintf(" | documents: %d", len(task.Documents))
		}
	}
	if detail {
		line += " | updated: " + task.UpdatedAt
	}
	return line
}

// renderTaskList is the result of a flat list call: the lines that fit max_list_bytes,
// and, when matches remain (cut by the limit or by the bytes), has_more and the offset
// of the next page.
func renderTaskList(callID string, listed TaskListResult, offset int, detail bool, settings sessionfs.NativeTaskSettings) (string, error) {
	lines := make([]string, 0, len(listed.Tasks))
	lineBytes := 0
	for _, task := range listed.Tasks {
		line := taskListLine(task, detail)
		nextBytes := lineBytes + len(line)
		if len(lines) > 0 {
			nextBytes++
		}
		if int64(nextBytes) > settings.MaxListBytes {
			break
		}
		lines = append(lines, line)
		lineBytes = nextBytes
	}
	information := []tools.Information{{Key: "action", Value: "list"}, {Key: "count", Value: len(lines)}, {Key: "total", Value: listed.Total}}
	if offset+len(lines) < listed.Total {
		information = append(information, tools.Information{Key: "has_more", Value: true}, tools.Information{Key: "next_offset", Value: offset + len(lines)})
	}
	options := tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: fmt.Sprintf("%d task%s listed", len(lines), pluralSuffix(len(lines))), Information: information}
	if len(lines) > 0 {
		options.Content = &tools.Content{Encoding: "utf8", Text: strings.Join(lines, "\n")}
	}
	return taskRender(options)
}

func taskActionFor(arguments map[string]any) (taskAction, bool) {
	value, ok := arguments["action"].(string)
	if !ok {
		return "", false
	}
	action := taskAction(value)
	switch action {
	case taskActionList, taskActionGet, taskActionAdd, taskActionUpdate, taskActionDone, taskActionDelete:
		return action, true
	default:
		return "", false
	}
}

func taskStatusValue(value any) (TaskStatus, bool) {
	status, ok := value.(string)
	if !ok {
		return "", false
	}
	value = TaskStatus(status)
	switch value {
	case TaskTodo, TaskInProgress, TaskBlocked, TaskDone:
		return value.(TaskStatus), true
	default:
		return "", false
	}
}

func taskPriorityValue(value any) (TaskPriority, bool) {
	priority, ok := value.(string)
	if !ok {
		return "", false
	}
	result := TaskPriority(priority)
	switch result {
	case TaskPriorityLow, TaskPriorityNormal, TaskPriorityHigh, TaskPriorityUrgent:
		return result, true
	default:
		return "", false
	}
}

func taskText(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func taskString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func validTaskText(value string, maximum int64) bool {
	return strings.TrimSpace(value) != "" && int64(len(utf16.Encode([]rune(value)))) <= maximum
}

func taskStringArray(value any, maximumItems, maximumChars int64) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || int64(len(items)) > maximumItems {
		return nil, false
	}
	values := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok || !validTaskText(text, maximumChars) {
			return nil, false
		}
		values[index] = text
	}
	return values, true
}

func taskNoExtra(arguments map[string]any, allowed ...string) bool {
	for key := range arguments {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func taskFailure(callID, code, summary string) string {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, code, summary)
}

// taskNotFound lists the ids of the tasks that are not done, so the agent can pick one
// instead of guessing. The store read them where the lookup failed.
func taskNotFound(callID, id string, miss *TaskMiss) string {
	information := []tools.Information{{Key: "task_id", Value: id}}
	if openIDs := taskOpenIDs(miss); openIDs != "" {
		information = append(information, tools.Information{Key: "open_ids", Value: openIDs})
	}
	return tools.ExecutionError(callID, tools.StatusNotFound, "task_not_found", "No task has that id", information...)
}

// taskOpenIDs is the open_ids value: the ids, then how many more are not listed.
func taskOpenIDs(miss *TaskMiss) string {
	if miss == nil || len(miss.OpenIDs) == 0 {
		return ""
	}
	text := strings.Join(miss.OpenIDs, ", ")
	if more := miss.OpenTotal - len(miss.OpenIDs); more > 0 {
		text += fmt.Sprintf(" (+%d more)", more)
	}
	return text
}

func taskStorageError(callID string, err error) string {
	var storageError *TaskStoreError
	if errors.As(err, &storageError) {
		return tools.ExecutionError(callID, tools.StatusError, "task_storage_error", storageError.Error())
	}
	return tools.ExecutionError(callID, tools.StatusError, "task_operation_failed", err.Error())
}

func taskInformation(task TaskRecord, action taskAction) []tools.Information {
	information := []tools.Information{
		{Key: "action", Value: string(action)},
		{Key: "task_id", Value: task.ID},
		{Key: "title", Value: task.Title},
		{Key: "status", Value: string(task.Status)},
		{Key: "priority", Value: string(task.Priority)},
	}
	if len(task.Tags) > 0 {
		information = append(information, tools.Information{Key: "tags", Value: task.Tags})
	}
	if len(task.Documents) > 0 {
		information = append(information, tools.Information{Key: "documents", Value: task.Documents})
	}
	return information
}

func renderTask(callID string, action taskAction, task TaskRecord, summary string) (string, error) {
	return taskRender(tools.ResultOptions{ID: callID, Status: tools.StatusOK, Summary: summary, Information: taskInformation(task, action)})
}

func taskRender(options tools.ResultOptions) (string, error) {
	result, err := tools.RenderToolResult(options)
	if err != nil {
		return tools.ExecutionError(options.ID, tools.StatusError, "tool_output_failed", "The task result could not be rendered"), nil
	}
	return result, nil
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// TaskStateContext is a bounded snapshot of the configured session tasks.
type TaskStateContext struct {
	Items   []TaskStateContextItem `json:"items"`
	HasMore bool                   `json:"has_more"`
	// Counts, set in summary mode, holds one entry per configured status that has
	// tasks; Items then lists only the detail statuses.
	Counts        []TaskStateContextCount `json:"counts,omitempty"`
	OmittedCounts int                     `json:"omitted_counts,omitempty"`
}

type TaskStateContextCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// TaskStateContextItem is the compact subset included in state-context snapshots.
type TaskStateContextItem struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Status    TaskStatus   `json:"status"`
	Priority  TaskPriority `json:"priority"`
	Tags      []string     `json:"tags"`
	UpdatedAt string       `json:"updated_at"`
}

func CollectTaskStateContext(source TaskStateSource, settings sessionfs.NativeTaskStateContextSettings) (*TaskStateContext, error) {
	if source == nil {
		return nil, fmt.Errorf("task statecontext store is not available")
	}
	statuses := make([]TaskStatus, 0, len(settings.Statuses))
	for _, status := range settings.Statuses {
		statuses = append(statuses, TaskStatus(status))
	}
	if settings.Mode == sessionfs.NativeStateContextModeSummary {
		return collectTaskSummary(source, settings, statuses)
	}
	listed, err := source.List(TaskListFilter{Statuses: statuses, Limit: int(settings.Limit)})
	if err != nil {
		return nil, err
	}
	items := make([]TaskStateContextItem, 0, len(listed.Tasks))
	for _, task := range listed.Tasks {
		items = append(items, taskStateContextItem(task))
	}
	return &TaskStateContext{Items: items, HasMore: listed.HasMore}, nil
}

func taskStateContextItem(task TaskRecord) TaskStateContextItem {
	return TaskStateContextItem{ID: task.ID, Title: task.Title, Status: task.Status, Priority: task.Priority, Tags: append([]string{}, task.Tags...), UpdatedAt: task.UpdatedAt}
}

// collectTaskSummary reads every configured task once, so the counts and the listed
// detail tasks agree.
func collectTaskSummary(source TaskStateSource, settings sessionfs.NativeTaskStateContextSettings, statuses []TaskStatus) (*TaskStateContext, error) {
	listed, err := source.List(TaskListFilter{Statuses: statuses, Limit: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(listed.Tasks))
	items := make([]TaskStateContextItem, 0)
	hasMore := false
	for _, task := range listed.Tasks {
		values = append(values, string(task.Status))
		if !slices.Contains(settings.DetailStatuses, string(task.Status)) {
			continue
		}
		if len(items) >= int(settings.Limit) {
			hasMore = true
			continue
		}
		items = append(items, taskStateContextItem(task))
	}
	counts := make([]TaskStateContextCount, 0)
	for _, count := range countStates(settings.Statuses, values) {
		counts = append(counts, TaskStateContextCount{Status: count.State, Count: count.Count})
	}
	return &TaskStateContext{Items: items, HasMore: hasMore, Counts: counts}, nil
}

func (state *TaskStateContext) StateContextItemCount() int {
	if state == nil {
		return 0
	}
	return len(state.Items) + len(state.Counts)
}

func (state *TaskStateContext) TrimStateContextItem() bool {
	if state == nil {
		return false
	}
	if len(state.Items) > 0 {
		state.Items = state.Items[:len(state.Items)-1]
		state.HasMore = true
		return true
	}
	if len(state.Counts) > 0 {
		state.Counts = state.Counts[:len(state.Counts)-1]
		state.OmittedCounts++
		return true
	}
	return false
}
