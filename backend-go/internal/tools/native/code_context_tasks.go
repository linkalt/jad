package native

import (
	"fmt"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// codeContextTasks answers a call without a task: the tasks of the storage, most recently
// written first, with their descriptions and entry counts. It reads only the task files,
// never the code, so it says nothing of staleness; list with task does.
func codeContextTasks(store codeContextStore, settings sessionfs.NativeCodeContextSettings, callID string) string {
	files, total, err := store.tasks(settings.MaxScanTasks)
	if err != nil {
		return codeContextStoreFailure(callID, err)
	}
	var output strings.Builder
	switch {
	case total == 0:
		return "code_context: no tasks yet; read a range with a task to start one\n"
	case total > len(files):
		fmt.Fprintf(&output, "code_context: the %d most recently written of %d tasks (max_scan_tasks); list one with task\n", len(files), total)
	default:
		fmt.Fprintf(&output, "code_context: %d tasks, most recently written first; list one with task\n", total)
	}
	for _, file := range files {
		document, err := store.load(file.task)
		if err != nil {
			fmt.Fprintf(&output, "%s: error: %s\n", file.task, oneLine(err.Error()))
			continue
		}
		fmt.Fprintf(&output, "%s: %d entries, generation %d", file.task, len(document.Entries), document.Generation)
		if document.LastUpdatedBy != "" {
			output.WriteString(", by " + document.LastUpdatedBy)
		}
		fmt.Fprintf(&output, ", %s\n", file.modified.UTC().Format(time.RFC3339))
		if document.Description != "" {
			output.WriteString(indentCodeContext(document.Description, "  ") + "\n")
		}
	}
	return output.String()
}

// codeContextTaskDescriptionLine is the task's description as get and list head their
// answer with, starting with a newline; empty when the task has none.
func codeContextTaskDescriptionLine(document codeContextDocument) string {
	if document.Description == "" {
		return ""
	}
	return "\ntask description: " + strings.ReplaceAll(document.Description, "\n", "\n  ")
}
