package native

import (
	"errors"
	"fmt"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// taskBatchLimit is the most ops one call may hold. Settings built by hand without
// the limit get the default.
func taskBatchLimit(settings sessionfs.NativeTaskSettings) int {
	if settings.MaxBatchOps < 1 {
		return int(sessionfs.DefaultNativeTaskMaxBatchOps)
	}
	return int(settings.MaxBatchOps)
}

// executeTaskBatch runs the ops of one call in order against one load of the store and
// writes once. It is all or nothing: the first op that breaks a rule or names a task
// that does not exist stops the batch before anything is written.
func executeTaskBatch(arguments map[string]any, execution tools.ExecutionContext, settings sessionfs.NativeTaskSettings, store *TaskStore) (string, error) {
	limit := taskBatchLimit(settings)
	items, isList := arguments["ops"].([]any)
	if !taskNoExtra(arguments, "ops") || !isList || len(items) == 0 {
		return taskBatchRefusal(execution.CallID, "invalid_batch", fmt.Sprintf("ops must be a list of 1 to %d task objects and cannot be combined with other fields", limit)), nil
	}
	if len(items) > limit {
		return taskBatchRefusal(execution.CallID, "batch_too_large", fmt.Sprintf("A batch holds at most %d ops", limit)), nil
	}
	calls := make([]taskCall, len(items))
	ops := make([]TaskOp, len(items))
	for index, item := range items {
		fields, isObject := item.(map[string]any)
		if !isObject {
			return taskBatchRefusal(execution.CallID, "invalid_batch", fmt.Sprintf("ops must be a list of 1 to %d task objects and cannot be combined with other fields", limit)), nil
		}
		call, failure := parseTaskCall(fields, settings)
		if failure != nil {
			return taskBatchFailure(execution.CallID, index, tools.StatusInvalidInput, failure.code, failure.summary), nil
		}
		calls[index], ops[index] = call, call.op
	}
	outcome, err := store.Apply(ops, settings)
	if err != nil {
		if outcome.Failed < 0 {
			// The store could not be read or written; no op is to blame.
			return taskStorageError(execution.CallID, err), nil
		}
		code, summary := "task_operation_failed", err.Error()
		var storageError *TaskStoreError
		if errors.As(err, &storageError) {
			code = "task_storage_error"
		}
		return taskBatchFailure(execution.CallID, outcome.Failed, tools.StatusError, code, summary), nil
	}
	if outcome.Miss != nil {
		id := ops[outcome.Failed].ID
		extra := []tools.Information{{Key: "task_id", Value: id}}
		if openIDs := taskOpenIDs(outcome.Miss); openIDs != "" {
			extra = append(extra, tools.Information{Key: "open_ids", Value: openIDs})
		}
		return taskBatchFailure(execution.CallID, outcome.Failed, tools.StatusNotFound, "task_not_found", "No task has that id", extra...), nil
	}

	// List sections share max_list_bytes; the line of an op that changed a task is
	// never cut, because the model needs every id and every outcome.
	remaining := settings.MaxListBytes
	lines := make([]string, 0, len(ops))
	for index, result := range outcome.Results {
		call := calls[index]
		number := index + 1
		task := result.Task
		switch call.op.Action {
		case taskActionAdd, taskActionUpdate:
			lines = append(lines, fmt.Sprintf("%d %s %s | %s | %s", number, call.op.Action, task.ID, task.Status, task.Priority))
		case taskActionDone, taskActionDelete:
			lines = append(lines, fmt.Sprintf("%d %s %s", number, call.op.Action, task.ID))
		case taskActionGet:
			lines = append(lines, fmt.Sprintf("%d get %s", number, taskListLine(task, false)))
		case taskActionList:
			shown := make([]string, 0, len(result.List.Tasks))
			for _, listed := range result.List.Tasks {
				line := "  " + taskListLine(listed, call.detail)
				if int64(len(line)+1) > remaining {
					break
				}
				remaining -= int64(len(line) + 1)
				shown = append(shown, line)
			}
			header := fmt.Sprintf("%d list %d of %d", number, len(shown), result.List.Total)
			if next := call.op.Filter.Offset + len(shown); next < result.List.Total {
				header += fmt.Sprintf(", next_offset %d", next)
			}
			lines = append(lines, header)
			lines = append(lines, shown...)
		}
	}
	verb := "run"
	if outcome.Wrote {
		verb = "applied"
	}
	return taskRender(tools.ResultOptions{
		ID: execution.CallID, Status: tools.StatusOK, Summary: fmt.Sprintf("%d ops %s", len(ops), verb),
		Information: []tools.Information{{Key: "action", Value: "batch"}, {Key: "count", Value: len(ops)}},
		Content:     &tools.Content{Encoding: "utf8", Text: strings.Join(lines, "\n")},
	})
}

// taskBatchRefusal is the result of a batch refused before any op ran.
func taskBatchRefusal(callID, code, summary string) string {
	return tools.ExecutionError(callID, tools.StatusInvalidInput, code, summary, tools.Information{Key: "applied", Value: 0})
}

// taskBatchFailure is the result of a batch stopped by op index (0-based here, 1-based in
// the result). Nothing was written.
func taskBatchFailure(callID string, index int, status tools.Status, code, message string, extra ...tools.Information) string {
	information := []tools.Information{
		{Key: "failed_op", Value: index + 1},
		{Key: "op_error_code", Value: code},
		{Key: "op_error", Value: message},
	}
	information = append(information, extra...)
	information = append(information, tools.Information{Key: "applied", Value: 0})
	entries := append([]tools.Information{{Key: "error_type", Value: "execution"}, {Key: "error_code", Value: "batch_failed"}}, information...)
	result, err := tools.RenderToolResult(tools.ResultOptions{ID: callID, Status: status, Summary: fmt.Sprintf("Op %d failed: nothing was applied", index+1), Information: entries})
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "tool_output_failed", "The task result could not be rendered")
	}
	return result
}
