package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf16"

	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/tools"
)

const taskToolID = "native.task"

var taskInputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["list","get","add","update","done","delete"],"description":"Optional when the supplied fields make the action clear."},
    "id":{"type":"string","minLength":1,"description":"Task id returned by add."},
    "title":{"type":"string","minLength":1,"description":"Task title for add or update."},
    "status":{"type":"string","enum":["todo","in-progress","blocked","done","all"],"description":"Task state; all is a list filter only."},
    "priority":{"type":"string","enum":["low","normal","high","urgent"],"description":"Optional task priority or list filter. Defaults to normal when adding."},
    "tags":{"type":"array","items":{"type":"string"},"description":"Task tags. An empty array clears tags on update."},
    "documents":{"type":"array","items":{"type":"string"},"description":"Planning-document paths. An empty array clears links on update."},
    "tag":{"type":"string","minLength":1,"description":"Optional tag filter for list."},
    "limit":{"type":"integer","minimum":1,"description":"Optional list limit; defaults to the session policy."}
  },
  "additionalProperties":false,
  "allOf":[
    {"if":{"properties":{"action":{"const":"add"}},"required":["action"]},"then":{"properties":{"title":{}},"required":["title"]}},
    {"if":{"properties":{"action":{"enum":["get","update","done","delete"]}},"required":["action"]},"then":{"properties":{"id":{}},"required":["id"]}},
    {"if":{"properties":{"action":{"const":"update"}},"required":["action"]},"then":{"anyOf":[
      {"properties":{"title":{}},"required":["title"]},
      {"properties":{"status":{}},"required":["status"]},
      {"properties":{"priority":{}},"required":["priority"]},
      {"properties":{"tags":{}},"required":["tags"]},
      {"properties":{"documents":{}},"required":["documents"]}
    ]}}
  ]
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
			Description: "Create and maintain session tasks.",
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
		normalized["id"] = strings.ToUpper(strings.TrimSpace(id))
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
	if _, exists := normalized["action"]; !exists {
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
	action, valid := taskActionFor(arguments)
	if !valid {
		return taskFailure(execution.CallID, "invalid_action", "Task action is invalid"), nil
	}
	id := taskText(arguments["id"])
	if id != "" {
		id = strings.ToUpper(id)
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
		return taskFailure(execution.CallID, "invalid_title", "Task title is empty or exceeds the configured length"), nil
	}
	if _, exists := arguments["tags"]; exists && !hasTags {
		return taskFailure(execution.CallID, "invalid_tags", "Task tags exceed the configured limits or contain empty values"), nil
	}
	if _, exists := arguments["documents"]; exists && !hasDocuments {
		return taskFailure(execution.CallID, "invalid_documents", "Task documents exceed the configured limits or contain empty values"), nil
	}

	switch action {
	case taskActionList:
		if !taskNoExtra(arguments, "action", "status", "priority", "tag", "limit") {
			return taskFailure(execution.CallID, "invalid_list", "List accepts only status, priority, tag, and limit filters"), nil
		}
		if _, exists := arguments["status"]; exists && arguments["status"] != "all" && !hasTaskStatus {
			return taskFailure(execution.CallID, "invalid_status", "Task status is invalid"), nil
		}
		if _, exists := arguments["priority"]; exists && !hasTaskPriority {
			return taskFailure(execution.CallID, "invalid_priority", "Task priority is invalid"), nil
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
		filter := TaskListFilter{Limit: limit}
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
		listed, err := store.List(filter)
		if err != nil {
			return taskStorageError(execution.CallID, err), nil
		}
		lines := make([]string, 0, len(listed.Tasks))
		lineBytes := 0
		for _, task := range listed.Tasks {
			line := fmt.Sprintf("%s | %s | %s | %s", task.ID, task.Status, task.Priority, task.Title)
			if len(task.Tags) > 0 {
				line += " | tags: " + strings.Join(task.Tags, ",")
			}
			if len(task.Documents) > 0 {
				line += fmt.Sprintf(" | documents: %d", len(task.Documents))
			}
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
		hasMore := listed.HasMore || len(lines) < len(listed.Tasks)
		information := []tools.Information{{Key: "action", Value: "list"}, {Key: "count", Value: len(lines)}}
		if hasMore {
			information = append(information, tools.Information{Key: "has_more", Value: true})
		}
		options := tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: fmt.Sprintf("%d task%s listed", len(lines), pluralSuffix(len(lines))), Information: information}
		if len(lines) > 0 {
			options.Content = &tools.Content{Encoding: "utf8", Text: strings.Join(lines, "\n")}
		}
		return taskRender(options)

	case taskActionAdd:
		if !taskNoExtra(arguments, "action", "title", "status", "priority", "tags", "documents") {
			return taskFailure(execution.CallID, "invalid_add", "Add accepts title, todo or in-progress status, priority, tags, and documents"), nil
		}
		if !hasTitle || title == "" {
			return taskFailure(execution.CallID, "missing_title", "Add requires a title"), nil
		}
		if _, exists := arguments["status"]; exists && (!hasTaskStatus || (taskStatus != TaskTodo && taskStatus != TaskInProgress)) {
			return taskFailure(execution.CallID, "invalid_creation_status", "New tasks must be todo or in-progress"), nil
		}
		if _, exists := arguments["priority"]; exists && !hasTaskPriority {
			return taskFailure(execution.CallID, "invalid_priority", "Task priority is invalid"), nil
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
		task, err := store.Create(TaskInput{Title: title, Status: taskStatus, Priority: taskPriority, Tags: tags, Documents: documents}, settings)
		if err != nil {
			return taskStorageError(execution.CallID, err), nil
		}
		return renderTask(execution.CallID, action, task, "Task added")

	default:
		if id == "" {
			return taskFailure(execution.CallID, "missing_task_id", "This task action requires an id"), nil
		}
		if !taskIDTextPattern.MatchString(id) {
			return taskFailure(execution.CallID, "invalid_task_id", "Task id is invalid"), nil
		}
		switch action {
		case taskActionGet:
			if !taskNoExtra(arguments, "action", "id") {
				return taskFailure(execution.CallID, "invalid_get", "Get accepts only an id"), nil
			}
			task, found, err := store.Get(id)
			if err != nil {
				return taskStorageError(execution.CallID, err), nil
			}
			if !found {
				return taskNotFound(execution.CallID, id, store, settings), nil
			}
			return renderTask(execution.CallID, action, task, "Task retrieved")

		case taskActionDelete:
			if !taskNoExtra(arguments, "action", "id") {
				return taskFailure(execution.CallID, "invalid_delete", "Delete requires exactly an id"), nil
			}
			deleted, err := store.Delete(id, settings)
			if err != nil {
				return taskStorageError(execution.CallID, err), nil
			}
			if !deleted {
				return taskNotFound(execution.CallID, id, store, settings), nil
			}
			return taskRender(tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: "Task deleted", Information: []tools.Information{{Key: "action", Value: string(action)}, {Key: "task_id", Value: id}}})

		case taskActionDone:
			if !taskNoExtra(arguments, "action", "id") {
				return taskFailure(execution.CallID, "invalid_done", "Done requires exactly an id"), nil
			}
			status := TaskDone
			task, found, err := store.Update(id, TaskPatch{Status: &status}, settings)
			if err != nil {
				return taskStorageError(execution.CallID, err), nil
			}
			if !found {
				return taskNotFound(execution.CallID, id, store, settings), nil
			}
			return renderTask(execution.CallID, action, task, "Task completed")

		case taskActionUpdate:
			if !taskNoExtra(arguments, "action", "id", "title", "status", "priority", "tags", "documents") {
				return taskFailure(execution.CallID, "invalid_update", "Update accepts title, status, priority, tags, and documents"), nil
			}
			if _, exists := arguments["status"]; exists && !hasTaskStatus {
				return taskFailure(execution.CallID, "invalid_status", "Task status is invalid"), nil
			}
			if _, exists := arguments["priority"]; exists && !hasTaskPriority {
				return taskFailure(execution.CallID, "invalid_priority", "Task priority is invalid"), nil
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
				return taskFailure(execution.CallID, "missing_update", "Update needs a title, status, priority, tags, or documents"), nil
			}
			task, found, err := store.Update(id, patch, settings)
			if err != nil {
				return taskStorageError(execution.CallID, err), nil
			}
			if !found {
				return taskNotFound(execution.CallID, id, store, settings), nil
			}
			return renderTask(execution.CallID, action, task, "Task updated")
		}
	}
	return taskFailure(execution.CallID, "invalid_action", "Task action is invalid"), nil
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

// taskNotFound lists the ids that exist, so the agent can pick one instead of guessing.
func taskNotFound(callID, id string, store *TaskStore, settings sessionfs.NativeTaskSettings) string {
	information := []tools.Information{{Key: "task_id", Value: id}}
	if listed, err := store.List(TaskListFilter{Status: "all", Limit: int(settings.ListLimit)}); err == nil && len(listed.Tasks) > 0 {
		ids := make([]string, 0, len(listed.Tasks))
		for _, task := range listed.Tasks {
			ids = append(ids, task.ID)
		}
		information = append(information, tools.Information{Key: "available_ids", Value: strings.Join(ids, ", ")})
	}
	return tools.ExecutionError(callID, tools.StatusNotFound, "task_not_found", "No task has that id", information...)
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
		if !containsState(settings.DetailStatuses, string(task.Status)) {
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
